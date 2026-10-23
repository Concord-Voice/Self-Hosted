#!/usr/bin/env python3
"""Fail-closed validators for the #2505 self-host acceptance observations.

This module deliberately accepts projections, not raw Docker/Compose output.
Callers must discard environment and command output before invoking it.
"""
from __future__ import annotations

import argparse
from dataclasses import asdict, dataclass
from decimal import Decimal, InvalidOperation
import hashlib
import io
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import secrets
import signal
import stat
import tarfile
import subprocess
import tempfile
import time
from typing import Any


_STARTUP_PHASES = frozenset({
    "startup_cp_build", "startup_cp_ready", "startup_mp_build", "startup_mp_ready",
    "startup_mp_liveness", "startup_mp_protocol", "startup_enforcement",
    "startup_postcheck", "startup_dependencies", "startup_remainder",
})


def _controller_phase_context(output: str) -> str:
    """Project the last exact known controller marker into a finite label."""
    markers = {
        "voice-enforcement-rollout: building the new control-plane image": "startup_cp_build",
        "voice-enforcement-rollout: starting the control-plane in compatibility mode": "startup_cp_ready",
        "voice-enforcement-rollout: building the new media-plane image": "startup_mp_build",
        "voice-enforcement-rollout: starting the new media-plane": "startup_mp_ready",
        "voice-enforcement-rollout: waiting for media-plane liveness": "startup_mp_liveness",
        "voice-enforcement-rollout: waiting for media-plane protocol 3 readiness": "startup_mp_protocol",
        "voice-enforcement-rollout: activating durable enforcement": "startup_enforcement",
        "voice-enforcement-rollout: completed": "startup_postcheck",
    }
    prefixes = {
        "rebuild: starting the voice services' dependencies: ": "startup_dependencies",
        "rebuild: starting the remaining services: ": "startup_remainder",
    }
    services = {"control-plane", "media-plane", "ops-agent", "coturn",
                "postgres", "redis", "nats", "minio"}
    context = "selected_stack"
    for line in output.splitlines():
        if line in markers:
            context = markers[line]
        else:
            for prefix, assertion in prefixes.items():
                if line.startswith(prefix):
                    names = line[len(prefix):].split()
                    if names and all(name in services for name in names):
                        context = assertion
    return context


def _created_media_daemon_context(error: Any) -> str:
    """Classify bounded owned State.Error without exporting its private text."""
    fallback = "startup_mp_created"
    if type(error) is not str:
        return "startup_mp_diag_invalid"
    try:
        size = len(error.encode("utf-8"))
    except UnicodeEncodeError:
        return "startup_mp_diag_invalid"
    if size > 4096:
        return "startup_mp_diag_limit"
    if not error or not error.isprintable():
        return fallback

    # Quoted paths/commands are opaque, and malformed quoting is not a cause.
    unquoted = []
    quote = None
    escaped = False
    for char in error:
        if quote is not None:
            unquoted.append(" ")
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == quote:
                quote = None
        elif char in {"'", '"'}:
            quote = char
            unquoted.append(" ")
        else:
            unquoted.append(char)
    if quote is not None:
        return fallback
    visible = "".join(unquoted)
    causes = {"permission denied": "permission", "operation not permitted": "permission",
              "cannot allocate memory": "resource", "resource temporarily unavailable": "resource",
              "no space left on device": "resource", "too many open files": "resource",
              "port is already allocated": "bind", "address already in use": "bind",
              "executable file not found in $PATH": "oci"}
    families = {family for reason, family in causes.items() if reason in visible}
    if len(families) > 1:
        return fallback

    network_prefix = ("failed to set up container networking: driver failed programming "
                      "external connectivity on endpoint concordvoice-media-plane ")
    network_pattern = re.escape(network_prefix) + r"\([0-9a-f]{64}\): "
    bound = re.fullmatch(network_pattern +
                         r"Bind for (\[[0-9A-Fa-f:.]+\]|[0-9.]+):"
                         r"([0-9]{1,5}) failed: port is already allocated", error)
    if bound is not None:
        try:
            ipaddress.ip_address(bound[1].strip("[]"))
        except ValueError:
            return fallback
        if 1 <= int(bound[2]) <= 65535:
            return "startup_mp_daemon_bind"
        return fallback

    # Moby 28's PortBinding.String prints populated numeric IP/port tuples.
    address_pattern = r"(?:\[[0-9A-Fa-f:.]+\]|[0-9.]+)"
    port_pattern = r"[1-9][0-9]{0,4}"
    modern = re.fullmatch(
        network_pattern + r"failed to bind host port(?: (?P<chosen>" + port_pattern + r"))? for "
        r"(?P<host>" + address_pattern + r"):(?P<start>" + port_pattern + r")"
        r"(?:-(?P<end>" + port_pattern + r"))?:"
        r"(?P<container>" + address_pattern + r"):(?P<target>" + port_pattern + r")"
        r"/(?:tcp|udp): address already in use", error)
    if modern is not None:
        for name in ("host", "container"):
            address = modern[name]
            try:
                parsed = ipaddress.ip_address(address.strip("[]"))
            except ValueError:
                return fallback
            if (parsed.version == 6) != address.startswith("["):
                return fallback
        start, target = int(modern["start"]), int(modern["target"])
        end = int(modern["end"]) if modern["end"] is not None else start
        if not (1 <= start <= end <= 65535 and 1 <= target <= 65535):
            return fallback
        if modern["chosen"] is not None and not start <= int(modern["chosen"]) <= end:
            return fallback
        return "startup_mp_daemon_bind"

    operation = ("OCI runtime create failed: runc create failed: unable to start "
                 "container process: error during container init: ")
    prefixes = ("failed to create task for container: failed to create shim task: " + operation,
                operation)
    prefix = next((item for item in prefixes if error.startswith(item)), None)
    if prefix is None:
        return fallback
    body = error[len(prefix):]
    for reason, family in causes.items():
        terminal = ": " + reason + ": unknown"
        if (family in {"permission", "resource"} and visible.endswith(terminal)
                and body.endswith(terminal) and body[:-len(terminal)].strip()):
            return "startup_mp_daemon_" + family
    if re.fullmatch(r'exec: "(?:[^"\\]|\\.)+": executable file not found in \$PATH: unknown', body):
        return "startup_mp_daemon_oci"
    return fallback


class AcceptanceError(Exception):
    """A fixed, non-sensitive acceptance failure identifier."""

    def __init__(self, code: str, returncode: int | None = None,
                 service: str | None = None, startup_phase: str | None = None):
        self.code = code
        self.returncode = returncode
        self.service = service
        self.startup_phase = (startup_phase if isinstance(startup_phase, str)
                              and startup_phase in _STARTUP_PHASES else None)
        super().__init__(code)


@dataclass(frozen=True)
class CommandResult:
    returncode: int
    stdout: str
    stderr: str
    startup_phase: str | None = None


def _installer_failure_context(result: CommandResult) -> str:
    """Recognize plain fixed guard lines; keep private installer output local."""
    fallback = "port_posture"
    if not isinstance(result.stdout, str) or not isinstance(result.stderr, str):
        return fallback
    try:
        size = len(result.stdout.encode("utf-8")) + len(result.stderr.encode("utf-8"))
    except UnicodeEncodeError:
        return fallback
    if size > 8192:
        return fallback
    for output in (result.stdout, result.stderr):
        # The shipped installer colors ancillary status messages. Remove only
        # its exact SGR tokens for validation, never for guard-line matching.
        plain = re.sub(r"\x1b\[(?:0;31|0;32|1;33|0;34|0)m", "", output)
        if any(char != "\n" and not char.isprintable() for char in plain):
            return fallback
    guards = {
        "installer: UDP socket inventory unavailable or unparseable": "installer_udp_inventory",
        "installer: required UDP ports already in use": "installer_udp_busy",
        "selfhost-storage: existing installation refused": "installer_existing_installation",
        "selfhost-storage: installation discovery unavailable": "installer_discovery_unavailable",
    }
    contexts = {guards[line] for line in result.stderr.split("\n") if line in guards}
    return next(iter(contexts)) if len(contexts) == 1 else fallback


class SubprocessExecutor:
    """Run bounded child commands with a small, clean environment."""

    def __init__(self, workspace: Path):
        self.workspace = workspace
        self.docker_config: Path | None = None

    def run(self, argv: list[str], *, timeout: int | float,
            input_text: str | None = None) -> CommandResult:
        env = {key: os.environ[key] for key in ("PATH", "HOME", "LANG", "LC_ALL", "TERM", "TMPDIR")
               if key in os.environ}
        if self.docker_config is not None:
            env["DOCKER_CONFIG"] = str(self.docker_config)
        # The server-version contract distinguishes LF from CRLF. Text-mode
        # universal newline translation would admit a malformed real response.
        docker_server_query = argv == ["docker", "version", "--format", "{{.Server.Version}}"]
        child_input = input_text.encode("utf-8") if docker_server_query and input_text is not None else input_text
        try:
            child = subprocess.Popen(argv, cwd=self.workspace, env=env,
                                     stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                     text=not docker_server_query, start_new_session=True)
            stdout, stderr = child.communicate(input=child_input, timeout=timeout)
        except subprocess.TimeoutExpired as exc:
            # Wrappers launch foreground descendants. End their owned process
            # group before returning; this cannot undo Docker-daemon work.
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            except OSError as error:
                raise AcceptanceError("COMMAND_FAILED", 124) from error
            try:
                child.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                child.stdout.close()
                child.stderr.close()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired as error:
                    raise AcceptanceError("COMMAND_FAILED", 124) from error
            # Partial output remains private; callers never serialize it.
            def private_text(value: str | bytes | None) -> str:
                if isinstance(value, bytes):
                    return value[:65536].decode("utf-8", errors="replace")
                return value[:65536] if isinstance(value, str) else ""
            full_stdout = (exc.stdout.decode("utf-8", errors="replace")
                           if isinstance(exc.stdout, bytes) else exc.stdout or "")
            phase = _controller_phase_context(full_stdout)
            return CommandResult(124, private_text(exc.stdout), private_text(exc.stderr),
                                 phase if phase in _STARTUP_PHASES else None)
        except (OSError, ValueError) as exc:
            raise AcceptanceError("COMMAND_FAILED", 127) from exc
        if docker_server_query:
            stdout = stdout.decode("utf-8", errors="replace")
            stderr = stderr.decode("utf-8", errors="replace")
        return CommandResult(child.returncode, stdout, stderr)


@dataclass(frozen=True, order=True)
class Binding:
    service: str
    host_ip: str
    published: int
    target: int
    protocol: str


# Preserve the actual executor type independently of the injectable CLI factory.
_SUBPROCESS_EXECUTOR_TYPE = SubprocessExecutor


_SELECTED = frozenset({
    "postgres", "redis", "nats", "coturn", "minio", "ops-agent",
    "control-plane", "media-plane",
})
_PUBLIC_SELECTED = _SELECTED - {"ops-agent"}
_NO_PORTS = frozenset({"postgres", "redis", "nats", "minio", "ops-agent"})
_HEALTHY = frozenset({"postgres", "redis", "nats", "coturn", "control-plane", "media-plane"})
_LIVE_STATE_ASSERTIONS = {
    "postgres": "live_postgres_state", "redis": "live_redis_state",
    "nats": "live_nats_state", "coturn": "live_coturn_state",
    "minio": "live_minio_state", "ops-agent": "live_ops_agent_state",
    "control-plane": "live_control_plane_state", "media-plane": "live_media_plane_state",
}
_PORT_RE = re.compile(r"^([0-9]{1,5})(?:-([0-9]{1,5}))?$")
_PROJECT_RE = re.compile(r"^[a-z0-9][a-z0-9_-]*$")
_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")
_VOLUME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$")
_CONTAINER_STATES = frozenset({"created", "running", "paused", "restarting",
                               "removing", "exited", "dead"})
_HEALTH_STATES = frozenset({"none", "starting", "healthy", "unhealthy"})
_SHA_RE = re.compile(r"^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$")
_RUN_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")
_VERSION_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}$")


def _fail(code: str, returncode: int | None = None) -> None:
    raise AcceptanceError(code, returncode)


def _mapping(value: Any, code: str, required: set[str] | None = None,
             allowed: set[str] | None = None) -> dict:
    if not isinstance(value, dict) or any(not isinstance(k, str) for k in value):
        _fail(code)
    if required is not None and not required.issubset(value):
        _fail(code)
    if allowed is not None and not set(value).issubset(allowed):
        _fail(code)
    return value


def _int_port(value: Any) -> int:
    if type(value) is not int or not 1 <= value <= 65535:
        _fail("RENDERED_PORT_INVALID")
    return value


def _expand(value: Any, code: str) -> tuple[int, ...]:
    if type(value) is int:
        start = end = value
    elif isinstance(value, str):
        match = _PORT_RE.fullmatch(value)
        if not match:
            _fail(code)
        start = int(match.group(1))
        end = int(match.group(2) or match.group(1))
    else:
        _fail(code)
    if not 1 <= start <= end <= 65535 or end - start > 4095:
        _fail(code)
    return tuple(range(start, end + 1))


def _required_bindings() -> set[Binding]:
    expected = {
        Binding("control-plane", "127.0.0.1", 8080, 8080, "tcp"),
        Binding("media-plane", "127.0.0.1", 3000, 3000, "tcp"),
        Binding("coturn", "0.0.0.0", 3478, 3478, "tcp"),
        Binding("coturn", "0.0.0.0", 3478, 3478, "udp"),
        Binding("coturn", "0.0.0.0", 5349, 5349, "tcp"),
    }
    expected.update(Binding("media-plane", "0.0.0.0", port, port, "udp")
                    for port in range(40000, 42000))
    expected.update(Binding("coturn", "0.0.0.0", port, port, "udp")
                    for port in range(49152, 49253))
    return expected


def _active_services(model: dict, project: str, *, public_bundle: bool = False) -> dict:
    _mapping(model, "RENDERED_MODEL_INVALID", {"services"})
    if not isinstance(project, str) or not _PROJECT_RE.fullmatch(project):
        _fail("RENDERED_IDENTITY_INVALID")
    if model.get("name") != project:
        _fail("RENDERED_IDENTITY_INVALID")
    services = model["services"]
    if not isinstance(services, dict) or any(not isinstance(k, str) for k in services):
        _fail("RENDERED_SERVICE_SET_INVALID")
    active = {}
    for name, cfg in services.items():
        _mapping(cfg, "RENDERED_SERVICE_INVALID")
        profiles = cfg.get("profiles", [])
        if not isinstance(profiles, list) or any(not isinstance(p, str) for p in profiles):
            _fail("RENDERED_SERVICE_INVALID")
        # The acceptance render selects the services profile. Declared tools in
        # other profiles are intentionally outside this selected model.
        if profiles and "services" not in profiles:
            continue
        active[name] = cfg
    if type(public_bundle) is not bool:
        _fail("RENDERED_SERVICE_SET_INVALID")
    selected = _PUBLIC_SELECTED if public_bundle else _SELECTED
    if set(active) != selected:
        _fail("RENDERED_SERVICE_SET_INVALID")
    return active


def _rendered_mounts(service: str, cfg: dict) -> None:
    volumes = cfg.get("volumes", [])
    if not isinstance(volumes, list):
        _fail("RENDERED_MOUNT_INVALID")
    if service == "coturn":
        if any(not isinstance(mount, dict) for mount in volumes):
            _fail("RENDERED_MOUNT_INVALID")
        cert_mounts = [mount for mount in volumes
                       if mount.get("target") == "/etc/coturn/certs"]
        if len(cert_mounts) != 1:
            _fail("RENDERED_MOUNT_INVALID")
        mount = _mapping(cert_mounts[0], "RENDERED_MOUNT_INVALID",
                         {"type", "source", "target", "read_only"})
        if (mount["type"] != "bind" or mount["source"] != "/opt/concord/certs/coturn"
                or mount["target"] != "/etc/coturn/certs" or mount["read_only"] is not True):
            _fail("RENDERED_MOUNT_INVALID")
    elif any(not isinstance(v, dict) for v in volumes):
        _fail("RENDERED_MOUNT_INVALID")


def _rendered_bindings(active: dict) -> set[Binding]:
    found: set[Binding] = set()
    raw_count = 0
    for service, cfg in active.items():
        _rendered_mounts(service, cfg)
        ports = cfg.get("ports", [])
        if not isinstance(ports, list):
            _fail("RENDERED_PORT_INVALID")
        raw_count += len(ports)
        if service in _NO_PORTS and ports:
            _fail("RENDERED_PORT_EXTRA")
        for item in ports:
            item = _mapping(item, "RENDERED_PORT_INVALID", {"target", "protocol"},
                            {"target", "published", "protocol", "host_ip", "mode", "name", "app_protocol"})
            protocol = item["protocol"]
            if protocol not in ("tcp", "udp"):
                _fail("RENDERED_PORT_INVALID")
            targets = _expand(item["target"], "RENDERED_PORT_INVALID")
            published_value = item.get("published")
            if published_value is None:
                _fail("RENDERED_PORT_INVALID")
            published = _expand(published_value, "RENDERED_PORT_INVALID")
            if len(targets) != len(published):
                _fail("RENDERED_PORT_INVALID")
            address = item.get("host_ip", "")
            if address == "":
                address = "0.0.0.0"
            if not isinstance(address, str) or address not in ("0.0.0.0", "127.0.0.1", "::", "::1"):
                _fail("RENDERED_PORT_INVALID")
            if address in ("::", "::1"):
                # The selected configuration is expected to declare wildcard
                # ports; IPv6 parity is assessed from live observations.
                _fail("RENDERED_PORT_EXTRA")
            for target, pub in zip(targets, published):
                if target != pub:
                    _fail("RENDERED_PORT_INVALID")
                binding = Binding(service, address, pub, target, protocol)
                if binding in found:
                    _fail("RENDERED_PORT_EXTRA")
                found.add(binding)
    expected = _required_bindings()
    if found - expected:
        _fail("RENDERED_PORT_EXTRA")
    if expected - found:
        _fail("RENDERED_PORT_MISSING")
    return found


def validate_rendered(model: dict, project: str, *, public_bundle: bool = False) -> set[Binding]:
    """Validate the active services profile and its exact external bindings."""
    active = _active_services(model, project, public_bundle=public_bundle)
    return _rendered_bindings(active)


def _live_mounts(service: str, mounts: Any) -> None:
    if not isinstance(mounts, list):
        _fail("LIVE_MOUNT_INVALID")
    if service == "coturn":
        if any(not isinstance(mount, dict) for mount in mounts):
            _fail("LIVE_MOUNT_INVALID")
        cert_mounts = [mount for mount in mounts
                       if mount.get("target") == "/etc/coturn/certs"]
        if len(cert_mounts) != 1:
            _fail("LIVE_MOUNT_INVALID")
        mount = _mapping(cert_mounts[0], "LIVE_MOUNT_INVALID",
                         {"type", "source", "target", "read_only"})
        if (mount["type"] != "bind" or mount["source"] != "/opt/concord/certs/coturn"
                or mount["target"] != "/etc/coturn/certs" or mount["read_only"] is not True):
            _fail("LIVE_MOUNT_INVALID")


def validate_live(containers: list[dict], project: str, rendered: dict,
                  *, public_bundle: bool = False) -> dict:
    """Validate bounded same-project container observations against rendering."""
    expected = validate_rendered(rendered, project, public_bundle=public_bundle)
    active_services = _active_services(rendered, project, public_bundle=public_bundle)
    expected_names = {}
    for service, cfg in active_services.items():
        configured_name = cfg.get("container_name")
        if configured_name is None:
            expected_names[service] = f"/{project}-{service}-1"
        elif isinstance(configured_name, str) and configured_name and len(configured_name) <= 255:
            expected_names[service] = "/" + configured_name.lstrip("/")
        else:
            _fail("RENDERED_IDENTITY_INVALID")
    if not isinstance(containers, list) or not isinstance(project, str) or not _PROJECT_RE.fullmatch(project):
        _fail("LIVE_IDENTITY_INVALID")
    by_service: dict[str, dict] = {}
    ids: set[str] = set()
    ports: set[Binding] = set()
    for raw in containers:
        item = _mapping(raw, "LIVE_IDENTITY_INVALID",
                        {"id", "name", "service", "project", "image", "state", "health",
                         "exit_code", "ports", "mounts"},
                        {"id", "name", "service", "project", "image", "state", "health",
                         "exit_code", "ports", "mounts"})
        ident, name, service = item["id"], item["name"], item["service"]
        if (not isinstance(ident, str) or not _ID_RE.fullmatch(ident) or ident in ids
                or item["project"] != project):
            _fail("LIVE_IDENTITY_INVALID")
        ids.add(ident)
        if not isinstance(service, str) or service not in active_services:
            _fail("LIVE_SERVICE_SET_INVALID")
        if service in by_service:
            _fail("LIVE_IDENTITY_INVALID")
        if not isinstance(name, str) or name != expected_names[service]:
            _fail("LIVE_IDENTITY_INVALID")
        if not isinstance(item["image"], str) or not item["image"] or len(item["image"]) > 255:
            _fail("LIVE_IDENTITY_INVALID")
        by_service[service] = item
        _live_mounts(service, item["mounts"])
        raw_ports = item["ports"]
        if not isinstance(raw_ports, list):
            _fail("LIVE_PORT_EXTRA")
        for port in raw_ports:
            port = _mapping(port, "LIVE_PORT_EXTRA", {"target", "published", "protocol", "host_ip"},
                            {"target", "published", "protocol", "host_ip"})
            target, published, protocol, address = (port["target"], port["published"],
                                                       port["protocol"], port["host_ip"])
            if (type(target) is not int or not 1 <= target <= 65535
                    or type(published) is not int or not 1 <= published <= 65535
                    or protocol not in ("tcp", "udp")
                    or address not in ("0.0.0.0", "127.0.0.1", "::")):
                _fail("LIVE_PORT_EXTRA")
            binding = Binding(service, address, published, target, protocol)
            if binding in ports:
                _fail("LIVE_PORT_EXTRA")
            ports.add(binding)
    if set(by_service) != set(active_services):
        _fail("LIVE_SERVICE_SET_INVALID")
    # Required wildcard publications must be complete on IPv4. IPv6 is only
    # admitted as a complete parity set for wildcard bindings; management ports
    # are strictly IPv4 loopback and cannot appear on IPv6.
    expected_ipv4 = {b for b in expected if b.host_ip != "0.0.0.0"}
    expected_wildcard = {b for b in expected if b.host_ip == "0.0.0.0"}
    actual_ipv4 = {b for b in ports if b.host_ip != "::"}
    actual_ipv6 = {Binding(b.service, "0.0.0.0", b.published, b.target, b.protocol)
                   for b in ports if b.host_ip == "::"}
    if actual_ipv4 - (expected_ipv4 | expected_wildcard):
        _fail("LIVE_PORT_EXTRA")
    if actual_ipv4 != expected_ipv4 | expected_wildcard:
        _fail("LIVE_PORT_MISSING")
    if actual_ipv6 - expected_wildcard:
        _fail("LIVE_PORT_EXTRA")
    if actual_ipv6 and actual_ipv6 != expected_wildcard:
        _fail("LIVE_PORT_MISSING")
    # Structural failures and every other service must fail before a caller
    # considers waiting for coturn's initial healthcheck to finish.
    for service in sorted(by_service, key=lambda name: name == "coturn"):
        item = by_service[service]
        state, health, code = item["state"], item["health"], item["exit_code"]
        if service in _HEALTHY:
            if state != "running" or health != "healthy" or code is not None:
                raise AcceptanceError("LIVE_STATE_INVALID", service=service)
        elif service == "minio":
            if state != "running" or health != "none" or code is not None:
                raise AcceptanceError("LIVE_STATE_INVALID", service=service)
        elif service == "ops-agent":
            if state != "exited" or health != "none" or type(code) is not int or code != 0:
                raise AcceptanceError("LIVE_STATE_INVALID", service=service)
    return {"status": "PASS", "bindings": [asdict(b) for b in sorted(ports)]}


_EVIDENCE_KEYS = {"schema", "candidate_sha", "run_id", "runner_os", "tool_versions",
                  "local_only", "checks", "public_checks"}
_RUNNER_OS = {"ubuntu-24.04"}
_TOOL_KEYS = {"docker", "compose"}
_STAGES = {"host", "tools", "render", "observe", "lifecycle", "privacy"}
_ASSERTIONS = set(_LIVE_STATE_ASSERTIONS.values()) | {"host_floors", "host_capacity", "tool_versions", "selected_stack", "port_posture",
               "live_coturn_starting", "live_coturn_unhealthy",
               "live_coturn_not_running", "live_coturn_health_invalid",
               "coturn_mount", "disabled_agent", "local_only", "data_survives_restart",
               "plain_down_preserves_volumes", "stopped_health", "evidence_safety",
               "delivered_tls_metadata", "served_tls_leaf", "nginx_vhost",
               "nginx_config", "nginx_service", "nginx_service_enable",
               "nginx_service_start", "nginx_service_active", "nginx_service_reload",
               "installer_udp_inventory", "installer_udp_busy",
               "installer_existing_installation", "installer_discovery_unavailable",
               "startup_dependencies",
               "startup_cp_build", "startup_cp_ready", "startup_mp_build",
               "startup_mp_ready", "startup_mp_liveness", "startup_mp_protocol",
               "startup_mp_oom", "startup_mp_unhealthy", "startup_mp_restarting",
               "startup_mp_exited", "startup_mp_created", "startup_mp_running",
               "startup_mp_daemon_bind", "startup_mp_daemon_permission",
               "startup_mp_daemon_resource", "startup_mp_daemon_oci",
               "startup_mp_log_permission", "startup_mp_log_readonly",
               "startup_mp_log_missing_file", "startup_mp_log_missing_module",
               "startup_mp_log_native_load", "startup_mp_log_memory",
               "startup_mp_log_resource",
               "startup_mp_diag_timeout", "startup_mp_diag_missing",
               "startup_mp_diag_daemon", "startup_mp_diag_command",
               "startup_mp_diag_limit", "startup_mp_diag_invalid",
               "startup_mp_diag_identity", "startup_mp_diag_unavailable",
               "startup_enforcement", "startup_remainder",
               "startup_postcheck", "startup_timeout"}
_STATUSES = {"PASS", "FAIL", "BLOCKED", "UNVERIFIED"}
_CODES = {"OK", "HOST_FLOOR_INVALID", "HOST_CAPACITY_INVALID", "TOOL_VERSION_INVALID", "RENDER_INVALID",
          "RENDERED_MODEL_INVALID", "RENDERED_IDENTITY_INVALID",
          "RENDERED_SERVICE_INVALID", "RENDERED_SERVICE_SET_INVALID",
          "RENDERED_PORT_MISSING", "RENDERED_PORT_EXTRA", "RENDERED_PORT_INVALID",
          "RENDERED_MOUNT_INVALID", "LIVE_IDENTITY_INVALID", "LIVE_SERVICE_SET_INVALID",
          "LIVE_STATE_INVALID", "LIVE_PORT_MISSING", "LIVE_PORT_EXTRA", "LIVE_MOUNT_INVALID",
          "EVIDENCE_INVALID", "EVIDENCE_PATH_INVALID", "DATA_SURVIVAL_INVALID",
          "DOWN_PRESERVATION_INVALID", "LOCAL_ONLY_INVALID", "COMMAND_FAILED",
          "GUARD_INSTALL_INVALID", "GUARD_LOG_INVALID", "GUARD_TLS_INVALID",
          "LIFECYCLE_HEALTH_INVALID", "LIFECYCLE_UNKNOWN_INVALID",
          "LIFECYCLE_LOGS_INVALID", "TLS_LEAF_INVALID", "ROUTE_INVALID",
          "HOST_PREREQUISITES_FAILED"}
_CODES.add("BUNDLE_IDENTITY_INVALID")
_BUNDLE_KEYS = {"schema", "alpha_sha", "public_sha", "bundle_sha256", "staging",
                "run_id", "archive_path", "manifest_path"}
_BUNDLE_EVIDENCE_KEYS = (_EVIDENCE_KEYS - {"candidate_sha"}) | {
    "alpha_sha", "public_sha", "bundle_sha256", "staging"}


def _canonical_hex(value: Any, length: int) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{%d}" % length, value) is not None


def _bundle_claims(value: dict, code: str) -> None:
    if (not _canonical_hex(value.get("alpha_sha"), 40)
            or not _canonical_hex(value.get("public_sha"), 40)
            or not _canonical_hex(value.get("bundle_sha256"), 64)
            or type(value.get("run_id")) is not int or value["run_id"] <= 0):
        _fail(code)
    stage = _mapping(value.get("staging"), code,
                     {"run_id", "parent_sha", "artifact_sha256"},
                     {"run_id", "parent_sha", "artifact_sha256"})
    if (type(stage["run_id"]) is not int or stage["run_id"] <= 0
            or not _canonical_hex(stage["parent_sha"], 40)
            or not _canonical_hex(stage["artifact_sha256"], 64)):
        _fail(code)


def _unique_json(data: bytes) -> dict:
    def pairs(items: list) -> dict:
        result = {}
        for key, value in items:
            if key in result:
                _fail("BUNDLE_IDENTITY_INVALID")
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=pairs)


def _regular_bytes(path: Path) -> bytes:
    if not path.is_absolute():
        _fail("BUNDLE_IDENTITY_INVALID")
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        if current.is_symlink():
            _fail("BUNDLE_IDENTITY_INVALID")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            _fail("BUNDLE_IDENTITY_INVALID")
        return stream.read()


def verify_bundle_identity(path: Path, workspace: Path) -> tuple[dict, set[str]]:
    """Bind adapter identities to actual archive, manifest and unpacked payload bytes."""
    code = "BUNDLE_IDENTITY_INVALID"
    try:
        identity = _mapping(_unique_json(_regular_bytes(path)), code, _BUNDLE_KEYS, _BUNDLE_KEYS)
        if identity["schema"] != "selfhost-bundle-identity/v1":
            _fail(code)
        _bundle_claims(identity, code)
        for key in ("archive_path", "manifest_path"):
            if not isinstance(identity[key], str):
                _fail(code)
        archive_path = Path(identity["archive_path"])
        archive_bytes = _regular_bytes(archive_path)
        if hashlib.sha256(archive_bytes).hexdigest() != identity["bundle_sha256"]:
            _fail(code)
        manifest_bytes = _regular_bytes(Path(identity["manifest_path"]))
        keys = {"schema", "tag", "version", "alpha_sha", "public_sha", "root", "profile", "files"}
        manifest = _mapping(_unique_json(manifest_bytes), code, keys, keys)
        version = manifest["version"]
        if (not isinstance(version, str) or re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version) is None
                or manifest["schema"] != "selfhost-bundle/v1"
                or manifest["tag"] != "selfhost-v" + version
                or manifest["root"] != "concord-selfhost-" + version
                or workspace.name != manifest["root"] or manifest["profile"] != "services"
                or any(manifest[key] != identity[key] for key in ("alpha_sha", "public_sha"))):
            _fail(code)
        files = _mapping(manifest["files"], code)
        if not files or list(files) != sorted(files):
            _fail(code)
        for name, digest in files.items():
            if (not name or Path(name).is_absolute() or ".." in Path(name).parts
                    or str(Path(name)) != name or "\\" in name
                    or any(ord(char) < 32 or ord(char) == 127 for char in name)
                    or name == "selfhost-manifest.json"
                    or not _canonical_hex(digest, 64)):
                _fail(code)
        expected = set(files) | {"selfhost-manifest.json"}
        archive_files = {}
        archive_modes = {}
        seen = set()
        with tarfile.open(fileobj=io.BytesIO(archive_bytes), mode="r:gz") as archive:
            for member in archive:
                name = member.name
                if (name in seen or Path(name).is_absolute() or ".." in Path(name).parts
                        or str(Path(name)) != name or "\\" in name
                        or any(ord(char) < 32 or ord(char) == 127 for char in name)
                        or not (member.isdir() or member.isfile())
                        or member.mode & 0o7000):
                    _fail(code)
                seen.add(name)
                root, separator, relative = name.partition("/")
                if root != manifest["root"] or (not separator and not member.isdir()):
                    _fail(code)
                if member.isfile():
                    if member.mode not in (0o644, 0o755):
                        _fail(code)
                    archive_modes[relative] = member.mode
                    stream = archive.extractfile(member)
                    if stream is None:
                        _fail(code)
                    with stream:
                        archive_files[relative] = stream.read()
                elif relative and not any(item.startswith(relative + "/") for item in expected):
                    _fail(code)
        if set(archive_files) != expected or archive_files["selfhost-manifest.json"] != manifest_bytes:
            _fail(code)
        actual = set()
        for item in workspace.rglob("*"):
            if item.is_symlink() or not (item.is_dir() or item.is_file()):
                _fail(code)
            if item.is_file():
                actual.add(str(item.relative_to(workspace)))
        if actual != expected:
            _fail(code)
        for name in expected:
            if stat.S_IMODE((workspace / name).lstat().st_mode) != archive_modes[name]:
                _fail(code)
            payload = _regular_bytes(workspace / name)
            if payload != archive_files[name] or (name in files and hashlib.sha256(payload).hexdigest() != files[name]):
                _fail(code)
        if ("infrastructure/deploy/selfhost-security-event-logging.sh" not in files
                or "infrastructure/deploy/provision-production.sh" in files):
            _fail(code)
        return identity, expected
    except (OSError, ValueError, TypeError, tarfile.TarError, json.JSONDecodeError):
        _fail(code)


def _validate_evidence(record: dict) -> dict:
    bundle = record.get("schema") == "selfhost-bundle-acceptance/v2"
    keys = _BUNDLE_EVIDENCE_KEYS if bundle else _EVIDENCE_KEYS
    record = _mapping(record, "EVIDENCE_INVALID", keys, keys)
    if bundle:
        _bundle_claims(record, "EVIDENCE_INVALID")
    elif type(record["schema"]) is not int or record["schema"] != 1:
        _fail("EVIDENCE_INVALID")
    if not bundle and (not isinstance(record["candidate_sha"], str) or not _SHA_RE.fullmatch(record["candidate_sha"])):
        _fail("EVIDENCE_INVALID")
    if not bundle and (not isinstance(record["run_id"], str) or not _RUN_RE.fullmatch(record["run_id"])):
        _fail("EVIDENCE_INVALID")
    if (not isinstance(record["runner_os"], str) or record["runner_os"] not in _RUNNER_OS
            or record["local_only"] is not True):
        _fail("EVIDENCE_INVALID")
    if record["public_checks"] != "UNVERIFIED":
        _fail("EVIDENCE_INVALID")
    versions = _mapping(record["tool_versions"], "EVIDENCE_INVALID", _TOOL_KEYS, _TOOL_KEYS)
    if set(versions) != _TOOL_KEYS or any(not isinstance(v, str) or not _VERSION_RE.fullmatch(v)
                                          for v in versions.values()):
        _fail("EVIDENCE_INVALID")
    checks = record["checks"]
    if not isinstance(checks, list) or not checks or len(checks) > 32:
        _fail("EVIDENCE_INVALID")
    normalized_checks = []
    for check in checks:
        check = _mapping(check, "EVIDENCE_INVALID", {"stage", "assertion", "status", "code"},
                         {"stage", "assertion", "status", "code"})
        if (any(not isinstance(check[k], str) for k in ("stage", "assertion", "status", "code"))
                or check["stage"] not in _STAGES or check["assertion"] not in _ASSERTIONS
                or check["status"] not in _STATUSES or check["code"] not in _CODES):
            _fail("EVIDENCE_INVALID")
        normalized_checks.append({k: check[k] for k in ("stage", "assertion", "status", "code")})
    result = {"schema": record["schema"], "run_id": record["run_id"],
            "runner_os": record["runner_os"], "tool_versions": dict(versions),
            "local_only": True, "checks": normalized_checks, "public_checks": "UNVERIFIED"}
    if bundle:
        result.update({key: record[key] for key in ("alpha_sha", "public_sha", "bundle_sha256", "staging")})
    else:
        result["candidate_sha"] = record["candidate_sha"]
    return result


def _safe_directory(path: Path) -> Path:
    try:
        absolute = path.absolute()
        current = Path(absolute.anchor)
        for part in absolute.parts[1:]:
            current = current / part
            mode = current.lstat().st_mode
            if stat.S_ISLNK(mode):
                _fail("EVIDENCE_PATH_INVALID")
            if current == absolute and not stat.S_ISDIR(mode):
                _fail("EVIDENCE_PATH_INVALID")
        entries = list(absolute.iterdir())
    except AcceptanceError:
        raise
    except (OSError, ValueError):
        _fail("EVIDENCE_PATH_INVALID")
    if any(p.name != "evidence.json" for p in entries):
        _fail("EVIDENCE_PATH_INVALID")
    target = absolute / "evidence.json"
    if target.exists() or target.is_symlink():
        try:
            mode = target.lstat().st_mode
        except OSError:
            _fail("EVIDENCE_PATH_INVALID")
        if not stat.S_ISREG(mode):
            _fail("EVIDENCE_PATH_INVALID")
    return absolute


def write_evidence(directory: Path, record: dict) -> Path:
    """Atomically write the finite schema to a private regular file."""
    safe = _validate_evidence(record)
    if not isinstance(directory, Path):
        _fail("EVIDENCE_PATH_INVALID")
    absolute = _safe_directory(directory)
    destination = absolute / "evidence.json"
    temporary = absolute / (".evidence-" + secrets.token_hex(12) + ".tmp")
    # O_EXCL and O_NOFOLLOW keep creation local and prevent attacker-selected
    # links from redirecting evidence. Mode is restrictive from first byte.
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = None
    try:
        fd = os.open(temporary, flags, 0o600)
        payload = (json.dumps(safe, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
        with os.fdopen(fd, "wb") as stream:
            fd = None
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        # Recheck destination immediately before replacement; symlinks and
        # non-regular files are never accepted as the output path.
        if destination.is_symlink() or (destination.exists() and not stat.S_ISREG(destination.lstat().st_mode)):
            _fail("EVIDENCE_PATH_INVALID")
        os.replace(temporary, destination)
        os.chmod(destination, 0o600, follow_symlinks=False)
        dir_fd = os.open(absolute, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except AcceptanceError:
        raise
    except (OSError, ValueError, TypeError):
        _fail("EVIDENCE_PATH_INVALID")
    finally:
        if fd is not None:
            os.close(fd)
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
    return destination


_BASE_FILES = ("docker-compose.yml", "docker-compose.production.yml")
_BLACKSMITH_RUNNER_PROFILE = "blacksmith-4vcpu-ubuntu-2404"


def _ci_runner_profile_valid(env) -> bool:
    """Closed CI compatibility profile; this is not vendor attestation.

    Protected workflow/run provenance independently establishes release
    authority. A generic self-hosted environment grants no admission here.
    """
    environment = env.get("RUNNER_ENVIRONMENT")
    profile = env.get("SELFHOST_ACCEPTANCE_RUNNER_PROFILE")
    if environment == "github-hosted":
        return profile is None
    return (environment == "self-hosted"
            and profile == _BLACKSMITH_RUNNER_PROFILE
            and env.get("GITHUB_REPOSITORY") == "Concord-Voice/Concord-Voice-Alpha")


_SELECTED_FILES = (*_BASE_FILES, "docker-compose.selfhost.yml")
_SERVICES = frozenset({"postgres", "redis", "nats", "coturn", "minio", "ops-agent",
                       "control-plane", "media-plane"})
_HOST_TIMEOUT = 30
_PRIVILEGED_TIMEOUT_RESERVE = 10
# Fixed isolated root launcher; neither command text nor deadline is evaluated.
# GNU timeout owns the privileged foreground group and escalates before the
# outer observer allowance. Already accepted daemon work is not canceled.
_PRIVILEGED_SUPERVISOR = """import math, os, sys, time
try:
    remaining = float(sys.argv[1]) - time.monotonic() - 10
except (ValueError, IndexError):
    raise SystemExit(124)
if not math.isfinite(remaining) or remaining <= 0 or len(sys.argv) < 3:
    raise SystemExit(124)
os.execv("/usr/bin/timeout", ["/usr/bin/timeout", "--signal=TERM", "--kill-after=5s",
        "--", str(remaining) + "s", *sys.argv[2:]])
"""
# The selected eight services include two 45-second shutdown grace periods;
# allow those and six default ten-second periods, plus bounded command overhead.
_STOP_TIMEOUT = 180
# Leave a minute inside the workflow step for reap/evidence, and separate
# prerequisite/upload reserves inside its 50-minute job budget.
_RUN_TIMEOUT = 2580
# Startups share the remaining run budget instead of an independent shorter cap.
_START_TIMEOUT = _RUN_TIMEOUT
_LIVE_READY_TIMEOUT = 180
_ROUTE_TIMEOUT = 15
_LOCAL_CERT = "/etc/letsencrypt/live/ci-selfhost.example.test/fullchain.pem"
_INSPECT_TEMPLATE = (
    '{"Id":{{json .Id}},"Name":{{json .Name}},'
    '"Config":{"Image":{{json .Config.Image}},"Labels":{'
    '"com.docker.compose.project":{{json (index .Config.Labels "com.docker.compose.project")}},'
    '"com.docker.compose.service":{{json (index .Config.Labels "com.docker.compose.service")}}}},'
    '"State":{"Status":{{json .State.Status}},"ExitCode":{{json .State.ExitCode}},'
    '"Health":{{with (index .State "Health")}}{"Status":{{json .Status}}}{{else}}null{{end}}},'
    '"Mounts":{{json .Mounts}},'
    '"NetworkSettings":{"Ports":{{json .NetworkSettings.Ports}}}}'
)


def _docker_version(value: str) -> str:
    """Validate the complete server response and project a canonical triplet."""
    match = re.fullmatch(
        r"((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))"
        r"(?:\+[A-Za-z0-9.+-]+)?\n?", value)
    if match is None:
        _fail("TOOL_VERSION_INVALID")
    return match[1]


def _version_tuple(value: str) -> tuple[int, ...]:
    match = re.match(r"^([0-9]+(?:\.[0-9]+){1,3})", value.strip().lstrip("v"))
    if not match:
        _fail("TOOL_VERSION_INVALID")
    return tuple(int(part) for part in match.group(1).split("."))


def _version_at_least(value: str, minimum: tuple[int, ...]) -> bool:
    actual = (*_version_tuple(value), 0, 0, 0, 0)[:len(minimum)]
    return actual >= minimum


def _safe_child_result(result: Any) -> CommandResult:
    if (not hasattr(result, "returncode") or type(result.returncode) is not int
            or not isinstance(getattr(result, "stdout", None), str)
            or not isinstance(getattr(result, "stderr", None), str)):
        _fail("COMMAND_FAILED")
    phase = getattr(result, "startup_phase", None)
    phase = phase if isinstance(phase, str) and phase in _STARTUP_PHASES else None
    return CommandResult(result.returncode, result.stdout, result.stderr, phase)


def _normalize_inspect(rows: Any) -> list[dict]:
    if not isinstance(rows, list):
        _fail("LIVE_IDENTITY_INVALID")
    normalized = []
    for row in rows:
        if not isinstance(row, dict):
            _fail("LIVE_IDENTITY_INVALID")
        config, state = row.get("Config"), row.get("State")
        if not isinstance(config, dict) or not isinstance(state, dict):
            _fail("LIVE_IDENTITY_INVALID")
        labels = config.get("Labels")
        if not isinstance(labels, dict):
            _fail("LIVE_IDENTITY_INVALID")
        status = state.get("Status")
        health_data = state.get("Health")
        health = health_data.get("Status", "none") if isinstance(health_data, dict) else "none"
        network = row.get("NetworkSettings")
        raw_ports = network.get("Ports") if isinstance(network, dict) else None
        if not isinstance(raw_ports, dict):
            _fail("LIVE_PORT_EXTRA")
        ports = []
        for key, bindings in raw_ports.items():
            match = re.fullmatch(r"([0-9]{1,5})/(tcp|udp)", key) if isinstance(key, str) else None
            if not match or bindings is None:
                if bindings is None:
                    continue
                _fail("LIVE_PORT_EXTRA")
            if not isinstance(bindings, list):
                _fail("LIVE_PORT_EXTRA")
            for binding in bindings:
                if not isinstance(binding, dict):
                    _fail("LIVE_PORT_EXTRA")
                host_ip = binding.get("HostIp")
                host_port = binding.get("HostPort")
                try:
                    target_num, published_num = int(match.group(1)), int(host_port)
                except (TypeError, ValueError):
                    _fail("LIVE_PORT_EXTRA")
                ports.append({"target": target_num, "published": published_num,
                              "protocol": match.group(2), "host_ip": host_ip})
        mounts = row.get("Mounts", [])
        if not isinstance(mounts, list):
            _fail("LIVE_MOUNT_INVALID")
        normalized_mounts = []
        for mount in mounts:
            if not isinstance(mount, dict):
                _fail("LIVE_MOUNT_INVALID")
            normalized_mounts.append({
                "type": "bind" if mount.get("Type") == "bind" else mount.get("Type"),
                "source": mount.get("Source"), "target": mount.get("Destination"),
                "read_only": mount.get("RW") is False,
            })
        normalized.append({
            "id": row.get("Id"), "name": row.get("Name"),
            "service": labels.get("com.docker.compose.service"),
            "project": labels.get("com.docker.compose.project"),
            "image": config.get("Image"), "state": status, "health": health,
            "exit_code": state.get("ExitCode") if status == "exited" else None,
            "ports": ports, "mounts": normalized_mounts,
        })
    return normalized


def _inspect_rows(output: str) -> list[dict]:
    try:
        parsed = json.loads(output)
        if isinstance(parsed, dict):
            return [parsed]
        if isinstance(parsed, list):
            return parsed
    except (TypeError, json.JSONDecodeError):
        pass
    try:
        rows = [json.loads(line) for line in output.splitlines() if line.strip()]
    except (TypeError, json.JSONDecodeError):
        _fail("LIVE_IDENTITY_INVALID")
    if any(not isinstance(row, dict) for row in rows):
        _fail("LIVE_IDENTITY_INVALID")
    return rows


class AcceptanceRunner:
    """Admitted, local-only fresh-host acceptance through shipped entrypoints."""

    def __init__(self, executor: Any, workspace: Path, evidence_directory: Path,
                 bundle_identity: Path | None = None):
        self.executor = executor
        self.workspace = workspace
        self.evidence_directory = evidence_directory
        self.checks: list[dict[str, str]] = []
        self.stage = "host"
        self.assertion = "host_floors"
        self.project: str | None = None
        self.host_cpus: int | None = None
        self.candidate_sha = os.environ.get("GITHUB_SHA", "")
        self.run_id = os.environ.get("GITHUB_RUN_ID", "")
        self.tool_versions = {"docker": "unavailable", "compose": "unavailable"}
        self.source_bytes: dict[str, tuple[int, str]] = {}
        self.deadline: float | None = None
        self.scratch: tempfile.TemporaryDirectory[str] | None = None
        self.bundle_identity_path = bundle_identity
        self.bundle_identity: dict | None = None
        self.bundle_paths: set[str] = set()

    def _record(self, stage: str, assertion: str, status: str, code: str = "OK") -> None:
        self.checks.append({"stage": stage, "assertion": assertion,
                            "status": status, "code": code})

    def _bounded_timeout(self, timeout: int | float) -> int | float:
        if self.deadline is None:
            return timeout
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise AcceptanceError("COMMAND_FAILED", 124)
        return min(timeout, remaining)

    def _execute(self, argv: list[str], timeout: int | float = _HOST_TIMEOUT,
                 input_text: str | None = None) -> CommandResult:
        bounded = self._bounded_timeout(timeout)
        if argv[:2] == ["sudo", "-n"]:
            if bounded <= _PRIVILEGED_TIMEOUT_RESERVE:
                raise AcceptanceError("COMMAND_FAILED", 124)
            deadline = time.monotonic() + bounded
            if self.deadline is not None:
                deadline = min(deadline, self.deadline)
            # Compute the inner allowance after entering the privilege boundary.
            # A delayed sudo transition must not grant a fresh relative budget.
            argv = ["sudo", "-n", "/usr/bin/python3", "-I", "-c",
                    _PRIVILEGED_SUPERVISOR, str(deadline), *argv[2:]]
        result = _safe_child_result(self.executor.run(
            argv, timeout=bounded, input_text=input_text))
        if self.deadline is not None and time.monotonic() >= self.deadline:
            raise AcceptanceError("COMMAND_FAILED", result.returncode or 124,
                                  startup_phase=result.startup_phase)
        return result

    def _run(self, argv: list[str], timeout: int | float = _HOST_TIMEOUT,
             input_text: str | None = None) -> CommandResult:
        try:
            result = self._execute(argv, timeout=timeout, input_text=input_text)
        except AcceptanceError:
            raise
        if result.returncode != 0:
            raise AcceptanceError("COMMAND_FAILED", result.returncode)
        return result

    def _expect_refusal(self, argv: list[str], diagnostic: str, code: str,
                        stage: str, assertion: str, *, require_empty: bool = True) -> None:
        self.stage, self.assertion = stage, assertion
        before = self._project_state()
        try:
            result = self._execute(argv, timeout=_HOST_TIMEOUT)
        except AcceptanceError:
            raise
        if (result.returncode <= 0 or result.returncode == 124
                or diagnostic not in (result.stdout + result.stderr)):
            raise AcceptanceError(code, result.returncode)
        after = self._project_state()
        if before != after or (require_empty and any(after)):
            raise AcceptanceError(code, result.returncode)
        self._record(stage, assertion, "PASS")

    def _evidence_record(self) -> dict:
        if self.bundle_identity is not None:
            return {"schema": "selfhost-bundle-acceptance/v2",
                    **{key: self.bundle_identity[key] for key in
                       ("alpha_sha", "public_sha", "bundle_sha256", "staging", "run_id")},
                    "runner_os": "ubuntu-24.04", "tool_versions": dict(self.tool_versions),
                    "local_only": True, "checks": list(self.checks), "public_checks": "UNVERIFIED"}
        sha = self.candidate_sha if isinstance(self.candidate_sha, str) and _SHA_RE.fullmatch(self.candidate_sha) else "0" * 40
        run_id = self.run_id if isinstance(self.run_id, str) and _RUN_RE.fullmatch(self.run_id) else "unavailable"
        return {"schema": 1, "candidate_sha": sha, "run_id": run_id,
                "runner_os": "ubuntu-24.04", "tool_versions": dict(self.tool_versions),
                "local_only": True, "checks": list(self.checks),
                "public_checks": "UNVERIFIED"}

    def _flush_evidence(self) -> None:
        if not self.checks:
            self._record("host", "host_floors", "FAIL", "HOST_FLOOR_INVALID")
        write_evidence(self.evidence_directory, self._evidence_record())

    def _admit_identity(self) -> None:
        self.stage, self.assertion = "host", "host_floors"
        env = os.environ
        if (platform.system() != "Linux" or not hasattr(os, "getuid") or os.getuid() == 0
                or env.get("GITHUB_ACTIONS") != "true"
                or not _ci_runner_profile_valid(env)
                or env.get("RUNNER_OS") != "Linux"
                or env.get("GITHUB_EVENT_NAME") != ("workflow_dispatch" if self.bundle_identity_path else "pull_request")
                or not _SHA_RE.fullmatch(env.get("GITHUB_SHA", ""))
                or not _RUN_RE.fullmatch(env.get("GITHUB_RUN_ID", ""))):
            _fail("HOST_FLOOR_INVALID")
        if not isinstance(self.workspace, Path) or not self.workspace.is_dir():
            _fail("HOST_FLOOR_INVALID")
        env_file = self.workspace / ".env"
        if env_file.exists() or env_file.is_symlink():
            _fail("HOST_FLOOR_INVALID")
        evidence = _safe_directory(self.evidence_directory)
        try:
            if list(evidence.iterdir()):
                _fail("EVIDENCE_PATH_INVALID")
        except OSError:
            _fail("EVIDENCE_PATH_INVALID")
        if self.bundle_identity_path is not None:
            self.bundle_identity, self.bundle_paths = verify_bundle_identity(self.bundle_identity_path, self.workspace)
            if (env.get("GITHUB_REF") != "refs/heads/main" or env.get("GITHUB_REF_NAME") != "main"
                    or env.get("GITHUB_RUN_ID") != str(self.bundle_identity["run_id"])):
                _fail("HOST_FLOOR_INVALID")

    def _host_facts(self) -> None:
        self.stage, self.assertion = "host", "host_floors"
        os_release = self._run(["cat", "/etc/os-release"]).stdout
        # Normalize only the two public OS identity fields.
        os_lines = dict()
        for line in os_release.splitlines():
            key, sep, value = line.partition("=")
            if sep and key in {"ID", "VERSION_ID"}:
                os_lines[key] = value.strip().strip('"')
        if os_lines.get("ID") != "ubuntu" or os_lines.get("VERSION_ID") != "24.04":
            _fail("HOST_FLOOR_INVALID")
        mem_text = self._run(["free", "-k"]).stdout
        mem_match = re.search(r"(?m)^Mem:\s+([0-9]+)", mem_text)
        meminfo_text = self._run(["cat", "/proc/meminfo"]).stdout
        meminfo = re.search(r"(?m)^MemTotal:\s+([0-9]+)\s+kB", meminfo_text)
        cpu_text = self._run(["nproc"]).stdout.strip()
        df_text = self._run(["df", "-Pk", str(self.workspace)]).stdout
        df_rows = [line.split() for line in df_text.splitlines() if line.strip()]
        try:
            cpus = int(cpu_text)
            memory_kb = int(meminfo.group(1)) if meminfo else 0
            free_kb = int(df_rows[1][3]) if len(df_rows) > 1 and len(df_rows[1]) >= 4 else 0
            memory_bytes = int(mem_match.group(1)) * 1024 if mem_match else memory_kb * 1024
        except (TypeError, ValueError, IndexError):
            _fail("HOST_FLOOR_INVALID")
        if cpus < 2 or memory_kb < 2_000_000 or free_kb < 10_000_000 or memory_bytes < 2_000_000_000:
            _fail("HOST_FLOOR_INVALID")
        docker_version = self._execute(["docker", "version", "--format", "{{.Server.Version}}"])
        if docker_version.returncode != 0:
            _fail("TOOL_VERSION_INVALID")
        self.tool_versions["docker"] = _docker_version(docker_version.stdout)
        self.tool_versions["compose"] = self._run(
            ["docker", "compose", "version", "--short"]
        ).stdout.strip().lstrip("v")
        if (not _VERSION_RE.fullmatch(self.tool_versions["docker"])
                or not _VERSION_RE.fullmatch(self.tool_versions["compose"])
                or not _version_at_least(self.tool_versions["docker"], (29, 8, 2))
                or not _version_at_least(self.tool_versions["compose"], (2, 24, 5))):
            _fail("TOOL_VERSION_INVALID")
        self._run(["logrotate", "--version"])
        info_text = self._run(["docker", "info", "--format", "{{json .}}"] ).stdout
        try:
            info = json.loads(info_text)
        except (TypeError, json.JSONDecodeError):
            _fail("HOST_FLOOR_INVALID")
        if (not isinstance(info, dict) or not isinstance(info.get("OSType"), str)
                or info["OSType"].lower() != "linux"):
            _fail("HOST_FLOOR_INVALID")
        if type(info.get("NCPU")) is not int or info["NCPU"] < 2:
            _fail("HOST_FLOOR_INVALID")
        self.host_cpus = info["NCPU"]
        context = self._run(["docker", "context", "inspect", "default",
                             "--format", "{{.Endpoints.docker.Host}}"] ).stdout.strip()
        if context != "unix:///var/run/docker.sock":
            _fail("HOST_FLOOR_INVALID")
        if self.bundle_identity is None:
            source_sha = self._run(["git", "rev-parse", "HEAD"]).stdout.strip()
            if source_sha.lower() != os.environ.get("GITHUB_SHA", "").lower():
                _fail("HOST_FLOOR_INVALID")
        self._record("host", "host_floors", "PASS")
        self._record("tools", "tool_versions", "PASS")

    def _project_is_empty(self) -> None:
        self.stage, self.assertion = "host", "selected_stack"
        containers, volumes = self._project_state()
        if containers or volumes:
            _fail("LIVE_IDENTITY_INVALID")
        self._record("host", "host_floors", "PASS")

    def _project_identity(self) -> None:
        self.stage, self.assertion = "host", "selected_stack"
        result = self._run(
            ["docker", "compose", "--project-directory", str(self.workspace),
             "--env-file", "/dev/null", "-f", "-", "config", "--format", "json"],
            input_text="services:\n  identity:\n    image: scratch\n")
        try:
            model = json.loads(result.stdout)
        except (TypeError, json.JSONDecodeError):
            _fail("HOST_FLOOR_INVALID")
        if not isinstance(model, dict):
            _fail("HOST_FLOOR_INVALID")
        project = model.get("name")
        if not isinstance(project, str) or not _PROJECT_RE.fullmatch(project):
            _fail("HOST_FLOOR_INVALID")
        self.project = project

    def _project_name(self) -> str:
        if self.project is None:
            _fail("HOST_FLOOR_INVALID")
        return self.project

    def _project_inventory(self) -> dict[str, str]:
        project = self._project_name()
        inventory = self._run(["docker", "ps", "-a", "--no-trunc", "--filter",
                                f"label=com.docker.compose.project={project}",
                                "--format", "json"]).stdout
        states: dict[str, str] = {}
        for row in _inspect_rows(inventory):
            row = _mapping(row, "LIVE_IDENTITY_INVALID")
            ident, state = row.get("ID"), row.get("State")
            if (not isinstance(ident, str) or not _ID_RE.fullmatch(ident)
                    or ident in states or not isinstance(state, str)
                    or state not in _CONTAINER_STATES):
                _fail("LIVE_IDENTITY_INVALID")
            states[ident] = state
        return states

    def _project_state(self) -> tuple[str, str]:
        project = self._project_name()
        inventory = self._project_inventory()
        # Docker's Status/RunningFor fields change with elapsed time even when
        # a refused command leaves every resource untouched. Compare stable
        # identities and states, never those presentation fields or row order.
        stable_inventory = sorted(inventory.items())
        containers = json.dumps(sorted(stable_inventory), separators=(",", ":")) if stable_inventory else ""
        volume_names = self._run(["docker", "volume", "ls", "-q", "--filter",
                                  f"label=com.docker.compose.project={project}"]).stdout.splitlines()
        if (any(not _VOLUME_RE.fullmatch(name) for name in volume_names)
                or len(set(volume_names)) != len(volume_names)):
            _fail("LIVE_IDENTITY_INVALID")
        volumes = "\n".join(sorted(volume_names))
        public_bundle = self.bundle_identity is not None
        services = _PUBLIC_SELECTED if public_bundle else _SERVICES
        names = [f"concordvoice-{service}" for service in sorted(services)]
        inspected = self._execute(
            ["docker", "inspect", "--type", "container", "--format",
             _INSPECT_TEMPLATE, *names], timeout=_HOST_TIMEOUT)
        if inspected.returncode == 0:
            rows = _normalize_inspect(_inspect_rows(inspected.stdout))
            identity = []
            inspect_ids: set[str] = set()
            for row in rows:
                ident, service = row["id"], row["service"]
                if (not isinstance(ident, str) or not _ID_RE.fullmatch(ident)
                        or ident in inspect_ids or not isinstance(service, str)
                        or service not in services or row["project"] != project
                        or row["name"] != f"/concordvoice-{service}"
                        or not isinstance(row["state"], str) or row["state"] not in _CONTAINER_STATES
                        or not isinstance(row["health"], str) or row["health"] not in _HEALTH_STATES
                        or (row["state"] == "exited" and type(row["exit_code"]) is not int)):
                    _fail("LIVE_IDENTITY_INVALID")
                inspect_ids.add(ident)
                identity.append((ident, row["name"], project, service,
                                 row["state"], row["health"], row["exit_code"]))
            if public_bundle and (set(inventory) != inspect_ids
                                  or {row["service"] for row in rows} != services):
                _fail("LIVE_SERVICE_SET_INVALID")
            if identity:
                containers += "\n" + json.dumps(sorted(identity), separators=(",", ":"))
        else:
            missing = []
            for line in inspected.stderr.splitlines():
                if not line.strip():
                    continue
                match = re.fullmatch(
                    r"(?:Error response from daemon: No such container: |"
                    r"Error: No such object: )(.+)", line)
                if match is None:
                    _fail("COMMAND_FAILED", inspected.returncode)
                missing.append(match.group(1))
            if (inspected.returncode != 1 or inspected.stdout.strip()
                    or sorted(missing) != sorted(names)):
                _fail("COMMAND_FAILED", inspected.returncode)
            if public_bundle and inventory:
                _fail("LIVE_SERVICE_SET_INVALID")
        return containers, volumes

    def _snapshot_sources(self) -> None:
        self.stage, self.assertion = "privacy", "evidence_safety"
        self._clean_tracked_tree()
        self.source_bytes = {name: self._source_identity(name) for name in self._tracked_paths()}

    def _clean_tracked_tree(self) -> None:
        if self.bundle_identity is not None:
            return
        if self._execute(["git", "diff", "--quiet", "HEAD", "--"]).returncode != 0:
            _fail("EVIDENCE_INVALID")

    def _tracked_paths(self) -> set[str]:
        if self.bundle_identity is not None:
            return set(self.bundle_paths)
        output = self._run(["git", "ls-files", "-z"]).stdout
        names = output[:-1].split("\0") if output.endswith("\0") else []
        if (not names or len(set(names)) != len(names)
                or any(not name or Path(name).is_absolute()
                       or ".." in Path(name).parts for name in names)):
            _fail("EVIDENCE_INVALID")
        return set(names)

    def _source_identity(self, name: str) -> tuple[int, str]:
        self._bounded_timeout(_HOST_TIMEOUT)
        path = self.workspace / name
        try:
            if not path.parent.resolve().is_relative_to(self.workspace.resolve()):
                _fail("EVIDENCE_INVALID")
            metadata = path.lstat()
            mode = metadata.st_mode
            digest = hashlib.sha256()
            if stat.S_ISLNK(mode):
                digest.update(os.fsencode(os.readlink(path)))
            elif stat.S_ISREG(mode):
                fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                with os.fdopen(fd, "rb") as stream:
                    mode = os.fstat(stream.fileno()).st_mode
                    if not stat.S_ISREG(mode):
                        _fail("EVIDENCE_INVALID")
                    while chunk := stream.read(1024 * 1024):
                        self._bounded_timeout(_HOST_TIMEOUT)
                        digest.update(chunk)
            else:
                _fail("EVIDENCE_INVALID")
            self._bounded_timeout(_HOST_TIMEOUT)
            return mode, digest.hexdigest()
        except (OSError, ValueError):
            _fail("EVIDENCE_INVALID")

    def _sources_unchanged(self) -> None:
        self.stage, self.assertion = "privacy", "evidence_safety"
        self._clean_tracked_tree()
        if (self._tracked_paths() != set(self.source_bytes)
                or any(self._source_identity(name) != content
                       for name, content in self.source_bytes.items())):
            _fail("EVIDENCE_INVALID")
        self._bounded_timeout(_HOST_TIMEOUT)
        self._record("privacy", "evidence_safety", "PASS")

    def _installer(self) -> None:
        self.stage, self.assertion = "render", "port_posture"
        if self.scratch is None:
            self.scratch = tempfile.TemporaryDirectory(prefix="selfhost-acceptance-",
                                                        dir=self.workspace)
        scratch = Path(self.scratch.name)
        invalid = scratch / "invalid.answers"
        invalid.write_text("CFG_DOMAIN=invalid\n", encoding="utf-8")
        invalid.chmod(0o600)
        invalid_result = self._execute(
            ["bash", str(self.workspace / "infrastructure/deploy/install-selfhost.sh"),
             "--env-file", str(invalid)], timeout=_HOST_TIMEOUT)
        env_file = self.workspace / ".env"
        if (invalid_result.returncode <= 0 or invalid_result.returncode == 124
                or "installer: invalid domain" not in invalid_result.stderr
                or env_file.exists() or env_file.is_symlink()):
            _fail("GUARD_INSTALL_INVALID", invalid_result.returncode)
        self._record("render", "evidence_safety", "PASS")
        answers = scratch / "literal-private.answers"
        answers.write_text(
            "CFG_DOMAIN=ci-selfhost.example.test\n"
            "CFG_TLS_MODE=local\n"
            "CFG_TURN_REALM=ci-selfhost.example.test\n"
            "CFG_PUBLIC_IP=127.0.0.1\n"
            "CFG_SMTP_HOST=mail.ci-selfhost.example.test\n"
            "CFG_SMTP_PORT=2525\n"
            "CFG_SMTP_USER=acceptance\n"
            "CFG_SMTP_PASS=synthetic-private-answer\n"
            "CFG_SMTP_FROM=acceptance@ci-selfhost.example.test\n"
            "CFG_STORAGE_MODE=bundled\n", encoding="utf-8")
        answers.chmod(0o600)
        installer = str(self.workspace / "infrastructure/deploy/install-selfhost.sh")
        installed = self._execute(["bash", installer, "--env-file", str(answers)])
        if installed.returncode != 0:
            self.assertion = _installer_failure_context(installed)
            raise AcceptanceError("COMMAND_FAILED", installed.returncode)
        mode = env_file.stat().st_mode & 0o777
        if mode != 0o600 or env_file.is_symlink():
            _fail("GUARD_INSTALL_INVALID")
        published = env_file.read_bytes()
        rerun = self._execute(
            ["bash", installer, "--env-file", str(answers)], timeout=_HOST_TIMEOUT)
        if (rerun.returncode <= 0 or rerun.returncode == 124
                or "selfhost-storage: existing installation refused" not in rerun.stderr
                or not env_file.is_file() or env_file.is_symlink()
                or env_file.read_bytes() != published
                or env_file.stat().st_mode & 0o777 != mode):
            _fail("GUARD_INSTALL_INVALID", rerun.returncode)
        # Keep no answer-file copy after the shipped installer has consumed it.
        answers.unlink(missing_ok=True)
        invalid.unlink(missing_ok=True)

    def _compose_model(self) -> dict:
        self.stage, self.assertion = "render", "selected_stack"
        project = self._project_name()
        base = ["docker", "compose", "-p", project]
        paths = [str(self.workspace / name) for name in _BASE_FILES]
        for path in paths:
            base.extend(["-f", path])
        self._run([*base, "config", "-q"])
        selected = ["docker", "compose", "-p", project]
        for name in _SELECTED_FILES:
            selected.extend(["-f", str(self.workspace / name)])
        selected.extend(["--profile", "services", "config", "--format", "json"])
        rendered_text = self._run(selected).stdout
        try:
            rendered = json.loads(rendered_text)
        except (TypeError, json.JSONDecodeError):
            _fail("RENDER_INVALID")
        validate_rendered(rendered, project, public_bundle=self.bundle_identity is not None)
        self._validate_host_capacity(rendered, project)
        self._record("render", "port_posture", "PASS")
        self._record("render", "coturn_mount", "PASS")
        return rendered

    def _validate_host_capacity(self, rendered: dict, project: str) -> None:
        self.stage, self.assertion = "render", "host_capacity"
        code = "HOST_CAPACITY_INVALID"
        if type(self.host_cpus) is not int or self.host_cpus < 2:
            _fail(code)
        quotas = {}
        for name, cfg in _active_services(
                rendered, project, public_bundle=self.bundle_identity is not None).items():
            deploy = _mapping(cfg.get("deploy"), code)
            resources = _mapping(deploy.get("resources"), code)
            limits = _mapping(resources.get("limits"), code)
            value = limits.get("cpus")
            if type(value) not in (str, int, float):
                _fail(code)
            try:
                quota = Decimal(str(value))
            except InvalidOperation:
                _fail(code)
            if not quota.is_finite() or quota <= 0 or quota > self.host_cpus:
                _fail(code)
            quotas[name] = quota
        environment = _mapping(rendered["services"]["media-plane"].get("environment"), code)
        workers = environment.get("NUM_WORKERS")
        if (type(workers) not in (str, int)
                or not re.fullmatch(r"[1-9][0-9]*", str(workers))
                or Decimal(str(workers)) > quotas["media-plane"]):
            _fail(code)
        # Limits apply independently to containers, rather than reserving a sum.
        self._record("render", "host_capacity", "PASS")

    def _wrapper(self, *args: str, timeout: int = _HOST_TIMEOUT) -> CommandResult:
        command = ["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost"), *args]
        if timeout == _START_TIMEOUT:
            try:
                result = self._execute(command, timeout=timeout)
            except AcceptanceError as exc:
                if exc.returncode == 124:
                    self.assertion = "startup_timeout"
                    if exc.startup_phase is not None:
                        self._record(self.stage, exc.startup_phase, "FAIL", "COMMAND_FAILED")
                raise
            if result.returncode != 0:
                if result.returncode == 124:
                    phase = result.startup_phase or _controller_phase_context(result.stdout)
                    if phase in _STARTUP_PHASES:
                        self._record(self.stage, phase, "FAIL", "COMMAND_FAILED")
                context = self._startup_failure_context(result)
                if context != "selected_stack":
                    self.assertion = context
                if self.assertion == "startup_mp_liveness":
                    self.assertion = self._media_start_failure_context()
                raise AcceptanceError("COMMAND_FAILED", result.returncode)
            return result
        return self._run(command, timeout=timeout)

    def _media_start_failure_context(self) -> str:
        """Best-effort owned metadata; diagnostics never replace startup failure."""
        fallback = "startup_mp_diag_unavailable"
        template = ('{"project":{{json (index .Config.Labels "com.docker.compose.project")}},'
                    '"service":{{json (index .Config.Labels "com.docker.compose.service")}},'
                    '"state":{{json .State}}}')
        try:
            inspected = self._execute(
                ["docker", "inspect", "--type", "container", "--format", template,
                 "concordvoice-media-plane"], timeout=8)
            size = len((inspected.stdout + inspected.stderr).encode("utf-8"))
            if size > 65536:
                return "startup_mp_diag_limit"
            if inspected.returncode == 124:
                return "startup_mp_diag_timeout"
            if inspected.returncode != 0:
                if inspected.returncode == 1 and not inspected.stdout.strip():
                    message = inspected.stderr.strip()
                    missing = {"Error response from daemon: No such container: concordvoice-media-plane",
                               "Error: No such object: concordvoice-media-plane"}
                    daemon = {"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
                              "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock"}
                    if message in missing:
                        return "startup_mp_diag_missing"
                    if message in daemon:
                        return "startup_mp_diag_daemon"
                return "startup_mp_diag_command"
            try:
                value = json.loads(inspected.stdout)
            except ValueError:
                return "startup_mp_diag_invalid"
            if not isinstance(value, dict):
                return "startup_mp_diag_invalid"
            if (value.get("project") != self._project_name()
                    or value.get("service") != "media-plane"):
                return "startup_mp_diag_identity"
            state = value.get("state")
            if (not isinstance(state, dict)
                    or any(type(state.get(key)) is not bool
                           for key in ("Running", "Restarting", "OOMKilled"))
                    or type(state.get("ExitCode")) is not int
                    or not 0 <= state["ExitCode"] <= 255
                    or state.get("Status") not in {"running", "restarting", "exited", "created"}):
                return "startup_mp_diag_invalid"
            health = state.get("Health")
            if health is not None and (not isinstance(health, dict)
                                      or health.get("Status") not in {"starting", "healthy", "unhealthy"}):
                return "startup_mp_diag_invalid"
            if state["OOMKilled"]:
                return "startup_mp_oom"
            if state["Restarting"]:
                context = "startup_mp_restarting"
            elif state["Status"] == "exited" and not state["Running"]:
                context = "startup_mp_exited"
            elif state["Status"] == "created" and not state["Running"]:
                context = _created_media_daemon_context(state.get("Error", ""))
                if context in {"startup_mp_diag_invalid", "startup_mp_diag_limit"}:
                    return context
            elif state["Status"] == "running" and state["Running"]:
                context = ("startup_mp_unhealthy" if health and health["Status"] == "unhealthy"
                           else "startup_mp_running")
            else:
                return "startup_mp_diag_invalid"
            # Keep validated state even if optional log collection fails.
            fallback = context
            logs = self._execute(
                ["docker", "logs", "--tail=20", "concordvoice-media-plane"], timeout=8)
            private = logs.stdout + "\n" + logs.stderr
            if size + len(private.encode("utf-8")) > 65536:
                return fallback
            if logs.returncode != 0:
                return context
            codes = {"EACCES": "permission", "EROFS": "readonly", "ENOENT": "missing_file",
                     "MODULE_NOT_FOUND": "missing_module", "ERR_MODULE_NOT_FOUND": "missing_module",
                     "ERR_DLOPEN_FAILED": "native_load", "ENOMEM": "memory",
                     "EAGAIN": "resource", "EMFILE": "resource", "ENFILE": "resource"}
            for line in private.splitlines():
                try:
                    record = json.loads(line)
                except ValueError:
                    continue
                if (not isinstance(record, dict) or record.get("message") != "Fatal error during startup"
                        or not isinstance(record.get("error"), dict)):
                    continue
                code = record["error"].get("code")
                if isinstance(code, str) and code in codes:
                    context = "startup_mp_log_" + codes[code]
            return context
        except Exception:
            # The original COMMAND_FAILED and child exit remain authoritative.
            return fallback

    @staticmethod
    def _startup_failure_context(result: CommandResult) -> str:
        """A timeout stays a timeout even when its finite phase is known."""
        if result.returncode == 124:
            return "startup_timeout"
        return _controller_phase_context(result.stdout)

    def _guarded_preparation(self) -> None:
        self._expect_refusal(
            ["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost"), "up"],
            "security-event log boundary missing: /var/log/concord-security",
            "GUARD_LOG_INVALID", "render", "local_only")
        preparation = (["selfhost-security-event-logging.sh", "security-event-logging"] if self.bundle_identity is not None
                       else ["provision-production.sh", "security-event-logging"])
        self._run(["sudo", "-n", "bash",
                   str(self.workspace / "infrastructure/deploy" / preparation[0]),
                   *preparation[1:]])
        self._expect_refusal(
            ["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost"), "up"],
            "self-host TLS material refused", "GUARD_TLS_INVALID", "render", "coturn_mount")
        self._run(["sudo", "-n", "bash",
                   str(self.workspace / "infrastructure/deploy/provision-cert.sh"),
                   "local", "--host", "ci-selfhost.example.test", "--ip", "127.0.0.1"])

    def _activate_nginx(self) -> None:
        """Install and activate the host vhost rendered by the shipped installer."""
        self.stage, self.assertion = "render", "nginx_vhost"
        rendered_vhost = self.workspace / "infrastructure/deploy/nginx/concordvoice.rendered.conf"
        if (not rendered_vhost.is_file() or rendered_vhost.is_symlink()
                or not rendered_vhost.resolve().is_relative_to(self.workspace.resolve())):
            _fail("ROUTE_INVALID")
        self._run(["sudo", "-n", "install", "-o", "root", "-g", "root", "-m", "0644",
                   str(rendered_vhost), "/etc/nginx/conf.d/concordvoice.conf"])
        self.assertion = "nginx_service_enable"
        self._run(["sudo", "-n", "systemctl", "enable", "nginx"])
        self.assertion = "nginx_config"
        self._run(["sudo", "-n", "nginx", "-t", "-c", "/etc/nginx/concord-selfhost-ci.conf"])
        self.assertion = "nginx_service_start"
        self._run(["sudo", "-n", "systemctl", "start", "nginx"])
        self.assertion = "nginx_service_active"
        self._run(["sudo", "-n", "systemctl", "is-active", "--quiet", "nginx"])
        self.assertion = "nginx_service_reload"
        self._run(["sudo", "-n", "systemctl", "reload", "nginx"])
        self._record("render", "local_only", "PASS")

    def _observe_live(self, rendered: dict) -> None:
        self.stage, self.assertion = "observe", "selected_stack"
        project = self._project_name()
        public_bundle = self.bundle_identity is not None
        services = _active_services(rendered, project, public_bundle=public_bundle)
        names = [cfg.get("container_name") for cfg in services.values()]
        if any(not isinstance(name, str) or not name for name in names):
            _fail("RENDERED_IDENTITY_INVALID")
        deadline = time.monotonic() + _LIVE_READY_TIMEOUT
        starting_error: AcceptanceError | None = None
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                if starting_error is not None:
                    self._record("observe", "live_coturn_starting", "FAIL", starting_error.code)
                    raise starting_error
                raise AcceptanceError("COMMAND_FAILED", 124)
            raw = self._run(["docker", "inspect", "--type", "container", "--format",
                             _INSPECT_TEMPLATE, *names], timeout=min(8, remaining))
            self.assertion = "selected_stack"
            containers = _normalize_inspect(_inspect_rows(raw.stdout))
            try:
                validate_live(containers, project, rendered, public_bundle=public_bundle)
            except AcceptanceError as exc:
                if exc.code != "LIVE_STATE_INVALID":
                    raise
                self.assertion = _LIVE_STATE_ASSERTIONS.get(exc.service, "selected_stack")
                if exc.service != "coturn":
                    raise
                # The validator checks all identities, mounts, ports and other
                # service states before returning this fixed service failure.
                coturn = next(item for item in containers if item["service"] == "coturn")
                if coturn["state"] != "running":
                    category = "live_coturn_not_running"
                elif coturn["health"] == "starting" and coturn["exit_code"] is None:
                    category = "live_coturn_starting"
                elif coturn["health"] == "unhealthy":
                    category = "live_coturn_unhealthy"
                else:
                    category = "live_coturn_health_invalid"
                remaining = deadline - time.monotonic()
                if category == "live_coturn_starting" and remaining > 0:
                    starting_error = exc
                    time.sleep(self._bounded_timeout(min(2, remaining)))
                    continue
                self._record("observe", category, "FAIL", exc.code)
                raise
            if time.monotonic() >= deadline:
                if starting_error is not None:
                    self.assertion = "live_coturn_state"
                    self._record("observe", "live_coturn_starting", "FAIL", starting_error.code)
                    raise starting_error
                raise AcceptanceError("COMMAND_FAILED", 124)
            break
        if public_bundle:
            self.assertion = "disabled_agent"
            # Named inspection alone cannot prove the inactive managed agent
            # stayed absent. Bind every same-project container, including
            # stopped containers, to the exact validated public runtime.
            if set(self._project_inventory()) != {item["id"] for item in containers}:
                _fail("LIVE_SERVICE_SET_INVALID")
        self._delivered_tls_metadata()
        self._record("observe", "selected_stack", "PASS")
        self._record("observe", "port_posture", "PASS")
        self._record("observe", "coturn_mount", "PASS")
        self._record("observe", "disabled_agent", "PASS")

    def _delivered_tls_metadata(self) -> None:
        """Verify the four delivered Coturn TLS objects without reading contents."""
        self.stage, self.assertion = "observe", "delivered_tls_metadata"
        result = self._run([
            "stat", "-c", "%F|%u|%g|%a|%s",
            "/opt/concord/certs", "/opt/concord/certs/coturn",
            "/opt/concord/certs/coturn/cert.pem",
            "/opt/concord/certs/coturn/key.pem",
        ])
        rows = result.stdout.splitlines()
        if len(rows) != 4:
            _fail("TLS_LEAF_INVALID")
        expected = (
            ("directory", 0, 0, 0o711),
            ("directory", 0, 0, 0o711),
            ("regular file", 0, 0, 0o444),
            ("regular file", 65534, 65533, 0o400),
        )
        for row, (kind, uid, gid, mode) in zip(rows, expected):
            match = re.fullmatch(r"([^|]+)\|([0-9]+)\|([0-9]+)\|([0-7]+)\|([0-9]+)", row)
            if (not match or match.group(1) != kind
                    or int(match.group(2)) != uid or int(match.group(3)) != gid
                    or int(match.group(4), 8) != mode or int(match.group(5)) <= 0):
                _fail("TLS_LEAF_INVALID")
        self._record("observe", "delivered_tls_metadata", "PASS")

    def _route(self, url: str, expected: str | dict[str, Any] | None, label: str,
               *, required_fields: dict[str, type] | None = None,
               exact_fields: bool = False) -> None:
        self.stage, self.assertion = "observe", "local_only"
        curl = ["curl", "--silent", "--show-error", "--include", "--max-time",
                str(_ROUTE_TIMEOUT), "--http1.1"]
        if url.startswith("https://"):
            host = url.split("/", 3)[2]
            curl.extend(["--resolve", f"{host}:443:127.0.0.1", "--cacert",
                         _LOCAL_CERT])
        curl.append(url)
        result = self._run(curl, timeout=_ROUTE_TIMEOUT + 5)
        lines = result.stdout.splitlines()
        split = re.split(r"\r?\n\r?\n", result.stdout, maxsplit=1)
        if len(split) == 2:
            response_headers, body = split
        else:
            response_headers = lines[0] if lines else ""
            body = "\n".join(lines[1:]) if lines else ""
        status_line = response_headers.splitlines()[0] if response_headers else ""
        body = body.rstrip("\r\n")
        body_valid = body == expected if isinstance(expected, str) else False
        if isinstance(expected, dict):
            try:
                payload = json.loads(body)
            except (TypeError, json.JSONDecodeError):
                payload = None
            if isinstance(payload, dict):
                body_valid = all(
                    type(payload.get(key)) is type(value) and payload.get(key) == value
                    for key, value in expected.items()
                ) and all(
                    key in payload and isinstance(payload[key], value_type)
                    for key, value_type in (required_fields or {}).items()
                )
                if exact_fields:
                    body_valid = body_valid and set(payload) == (
                        set(expected) | set(required_fields or {})
                    )
        if expected is None:
            body_valid = False
            if body.startswith("0"):
                try:
                    packet = json.loads(body[1:])
                except (TypeError, json.JSONDecodeError):
                    packet = None
                if isinstance(packet, dict):
                    sid = packet.get("sid")
                    upgrades = packet.get("upgrades")
                    body_valid = (
                        isinstance(sid, str) and bool(sid.strip())
                        and isinstance(upgrades, list)
                        and all(isinstance(upgrade, str) for upgrade in upgrades)
                        and all(type(packet.get(field)) is int and packet[field] > 0
                                for field in ("pingInterval", "pingTimeout", "maxPayload"))
                    )
        if not status_line.startswith("HTTP/1.1 200") or not body_valid:
            _fail("ROUTE_INVALID")

    def _tls_leaf(self) -> None:
        self.stage, self.assertion = "observe", "served_tls_leaf"
        source = self._run(["openssl", "x509", "-in",
                            _LOCAL_CERT, "-noout",
                            "-fingerprint", "-sha256"])
        source_match = re.search(r"(?i)Fingerprint=([A-F0-9:]+)", source.stdout)
        if not source_match:
            _fail("TLS_LEAF_INVALID")
        probe_argv = ["openssl", "s_client", "-connect", "127.0.0.1:5349",
                      "-servername", "turn.ci-selfhost.example.test",
                      "-verify_hostname", "turn.ci-selfhost.example.test",
                      "-CAfile", _LOCAL_CERT,
                      "-verify_return_error", "-showcerts"]
        served = self._run(probe_argv, timeout=_ROUTE_TIMEOUT)
        self.stage = "observe"
        fingerprint = self._run(["openssl", "x509", "-noout", "-fingerprint", "-sha256"],
                                input_text=served.stdout)
        served_match = re.search(r"(?i)Fingerprint=([A-F0-9:]+)", fingerprint.stdout)
        if not served_match or source_match.group(1).upper() != served_match.group(1).upper():
            _fail("TLS_LEAF_INVALID")
        self._record("observe", "served_tls_leaf", "PASS")

    def _local_observations(self) -> None:
        self.stage, self.assertion = "observe", "local_only"
        # Keep each route independent: the direct app probes and the two
        # nginx-owned responses prove separate local boundaries.
        cp_health = {"status": "healthy", "service": "control-plane"}
        mp_health = {"status": "healthy", "service": "media-plane"}
        self._route("http://127.0.0.1:8080/health", cp_health, "control-local",
                    exact_fields=True)
        self._route("http://127.0.0.1:8080/readyz",
                    {"status": "ready", "service": "control-plane"}, "control-ready",
                    required_fields={"checks": list}, exact_fields=True)
        self._route("http://127.0.0.1:3000/health", mp_health, "media-local")
        self._route("http://127.0.0.1:3000/readyz",
                    {"ready": True, "voiceEnforcementProtocol": 3}, "media-ready",
                    exact_fields=True)
        self._route("https://api.ci-selfhost.example.test/health", cp_health, "nginx-api",
                    exact_fields=True)
        self._route("https://api.ci-selfhost.example.test/nginx-health", "ok", "nginx-owned")
        self._route("https://media.ci-selfhost.example.test/socket.io/?EIO=4&transport=polling",
                    None, "nginx-media")
        self._tls_leaf()
        self._record("observe", "local_only", "PASS")

    def _database_row(self) -> None:
        self.stage, self.assertion = "lifecycle", "data_survives_restart"
        compose = ["docker", "compose", "-p", self._project_name()]
        for name in _SELECTED_FILES:
            compose.extend(["-f", str(self.workspace / name)])
        insert_sql = ("CREATE TABLE IF NOT EXISTS selfhost_acceptance_2505 "
                      "(id text PRIMARY KEY); INSERT INTO selfhost_acceptance_2505 "
                      "(id) VALUES ('owned-row') ON CONFLICT (id) DO NOTHING;")
        self._run([*compose, "exec", "-T", "postgres", "psql", "-U", "concord",
                   "-d", "concord", "-c", insert_sql])
        select_sql = "SELECT id FROM selfhost_acceptance_2505 WHERE id = 'owned-row';"
        initial = self._run([*compose, "exec", "-T", "postgres", "psql", "-U",
                             "concord", "-d", "concord", "-t", "-A", "-c", select_sql])
        if initial.stdout.strip() != "owned-row":
            _fail("DATA_SURVIVAL_INVALID")
        for verb in ("stop", "down"):
            if verb == "stop":
                self.stage, self.assertion = "lifecycle", "stopped_health"
                self._wrapper("stop", timeout=_STOP_TIMEOUT)
                stopped = self._execute(
                    ["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost"), "health"],
                    timeout=_HOST_TIMEOUT)
                if (stopped.returncode <= 0 or stopped.returncode == 124
                        or "control-plane unhealthy" not in (stopped.stdout + stopped.stderr)):
                    _fail("LIFECYCLE_HEALTH_INVALID", stopped.returncode)
                self._record("lifecycle", "stopped_health", "PASS")
            self.stage, self.assertion = "lifecycle", "data_survives_restart"
            self._wrapper("up", timeout=_START_TIMEOUT)
            readback = self._run([*compose, "exec", "-T", "postgres", "psql", "-U",
                                  "concord", "-d", "concord", "-t", "-A", "-c", select_sql])
            if readback.stdout.strip() != "owned-row":
                _fail("DATA_SURVIVAL_INVALID")
            self._record("lifecycle", "data_survives_restart", "PASS")
            if verb == "stop":
                self.stage, self.assertion = "lifecycle", "plain_down_preserves_volumes"
                self._wrapper("down", timeout=_STOP_TIMEOUT)
        self._record("lifecycle", "plain_down_preserves_volumes", "PASS")

    def _lifecycle(self) -> None:
        self.stage, self.assertion = "lifecycle", "data_survives_restart"
        self._wrapper("health")
        self._record("lifecycle", "selected_stack", "PASS")
        self._wrapper("status")
        self._record("lifecycle", "selected_stack", "PASS")
        log_sample = self._execute(
            ["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost"),
             "logs", "--no-follow", "--tail=20", "postgres"], timeout=8)
        if log_sample.returncode != 0 or not log_sample.stdout.strip():
            raise AcceptanceError("LIFECYCLE_LOGS_INVALID", log_sample.returncode)
        self._record("lifecycle", "evidence_safety", "PASS")
        self._expect_refusal(["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost"),
                              "logs", "not-a-service"], "selfhost-storage: unknown log service",
                             "LIFECYCLE_LOGS_INVALID", "lifecycle", "evidence_safety",
                             require_empty=False)
        self._expect_refusal(["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost"),
                              "unknown"], "unknown verb: unknown",
                             "LIFECYCLE_UNKNOWN_INVALID", "lifecycle", "evidence_safety",
                             require_empty=False)
        self._database_row()

    def run(self) -> dict:
        self.deadline = time.monotonic() + _RUN_TIMEOUT
        try:
            self._admit_identity()
            if isinstance(self.executor, _SUBPROCESS_EXECUTOR_TYPE):
                self.scratch = tempfile.TemporaryDirectory(prefix="selfhost-acceptance-",
                                                            dir=self.workspace)
                docker_config = Path(self.scratch.name) / "docker-config"
                docker_config.mkdir(mode=0o700)
                self.executor.docker_config = docker_config
            self._host_facts()
            self._project_identity()
            self._project_is_empty()
            self._snapshot_sources()
            self._installer()
            rendered = self._compose_model()
            self._guarded_preparation()
            self._activate_nginx()
            self.stage, self.assertion = "observe", "selected_stack"
            self._wrapper("up", timeout=_START_TIMEOUT)
            self._observe_live(rendered)
            self._local_observations()
            self._lifecycle()
            self._sources_unchanged()
            self._record("privacy", "evidence_safety", "PASS")
            record = self._evidence_record()
            self._flush_evidence()
            self._bounded_timeout(_HOST_TIMEOUT)
            return record
        except AcceptanceError as exc:
            self._record(self.stage, self.assertion, "FAIL", exc.code)
            self._flush_evidence()
            raise
        finally:
            if self.scratch is not None:
                self.scratch.cleanup()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run admitted local self-host acceptance checks")
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--fresh-host", action="store_true")
    mode.add_argument("--blocked-prerequisite", choices=("host-packages",))
    parser.add_argument("--evidence-dir", required=True)
    parser.add_argument("--bundle-identity")
    args = parser.parse_args(argv)
    workspace = Path.cwd()
    runner_temp = os.environ.get("RUNNER_TEMP")
    evidence = Path(args.evidence_dir).absolute()
    try:
        if not runner_temp:
            _fail("HOST_FLOOR_INVALID")
        temp_root = Path(runner_temp).absolute()
        relative_evidence = evidence.relative_to(temp_root)
        if not relative_evidence.parts:
            _fail("EVIDENCE_PATH_INVALID")
        if args.bundle_identity:
            if not evidence.exists():
                evidence.mkdir(mode=0o700, parents=True)
            runner = AcceptanceRunner(SubprocessExecutor(workspace), workspace, evidence,
                                      Path(args.bundle_identity))
            if args.blocked_prerequisite:
                try:
                    runner._admit_identity()
                    runner._record("host", "host_floors", "BLOCKED", "HOST_PREREQUISITES_FAILED")
                except AcceptanceError as exc:
                    runner._record("host", "host_floors", "FAIL", exc.code)
                    runner._flush_evidence()
                    raise
                runner._flush_evidence()
                print("selfhost-acceptance: BLOCKED HOST_PREREQUISITES_FAILED")
                return 0
            runner.run()
            print("selfhost-acceptance: PASS LOCAL_ONLY; public checks UNVERIFIED")
            return 0
        if args.blocked_prerequisite:
            env = os.environ
            sha = env.get("GITHUB_SHA", "")
            run_id = env.get("GITHUB_RUN_ID", "")
            if (platform.system() != "Linux" or not hasattr(os, "getuid") or os.getuid() == 0
                    or env.get("GITHUB_ACTIONS") != "true"
                    or not _ci_runner_profile_valid(env)
                    or env.get("RUNNER_OS") != "Linux"
                    or env.get("GITHUB_EVENT_NAME") != "pull_request"
                    or not _SHA_RE.fullmatch(sha)
                    or not _RUN_RE.fullmatch(run_id)
                    or not workspace.is_dir()):
                _fail("HOST_FLOOR_INVALID")
            result: CommandResult | None
            try:
                result = SubprocessExecutor(workspace).run(
                    ["git", "rev-parse", "HEAD"], timeout=_HOST_TIMEOUT)
            except AcceptanceError:
                result = None
            candidate = result.stdout.strip() if result is not None else ""
            code = ("HOST_PREREQUISITES_FAILED"
                    if result is not None and result.returncode == 0
                    and _SHA_RE.fullmatch(candidate)
                    and candidate.lower() == sha.lower()
                    else "HOST_FLOOR_INVALID")
            record = {
                "schema": 1,
                "candidate_sha": sha,
                "run_id": run_id,
                "runner_os": "ubuntu-24.04",
                "tool_versions": {"docker": "unavailable", "compose": "unavailable"},
                "local_only": True,
                "checks": [{"stage": "host", "assertion": "host_floors",
                            "status": "BLOCKED", "code": code}],
                "public_checks": "UNVERIFIED",
            }
            if not evidence.exists():
                evidence.mkdir(mode=0o700, parents=True)
            write_evidence(evidence, record)
            if code == "HOST_PREREQUISITES_FAILED":
                print("selfhost-acceptance: BLOCKED HOST_PREREQUISITES_FAILED")
                return 0
            print("selfhost-acceptance: FAIL HOST_FLOOR_INVALID")
            return 1
        if args.fresh_host:
            if not evidence.exists():
                evidence.mkdir(mode=0o700, parents=True)
        if not evidence.exists():
            _fail("EVIDENCE_PATH_INVALID")
        result = AcceptanceRunner(SubprocessExecutor(workspace), workspace, evidence).run()
    except AcceptanceError as exc:
        print(f"selfhost-acceptance: FAIL {exc.code}")
        return 1
    except (OSError, ValueError):
        print("selfhost-acceptance: FAIL EVIDENCE_PATH_INVALID")
        return 1
    print("selfhost-acceptance: PASS LOCAL_ONLY; public checks UNVERIFIED")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
