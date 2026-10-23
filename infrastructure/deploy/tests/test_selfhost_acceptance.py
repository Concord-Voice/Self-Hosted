#!/usr/bin/env python3
"""Pure validator and evidence contract tests for self-host acceptance (#2505).

These fixtures are owned projections of the selected Compose model and Docker
inspection data. They deliberately contain no container Env, command output,
credentials, or host data.
"""
from __future__ import annotations

import importlib.util
import hashlib
import io
import copy
from collections import Counter
import ipaddress
import json
import math
import os
import signal
import shutil
import subprocess
import time
from types import SimpleNamespace
from pathlib import Path
import re
import sys
import tempfile
import tarfile
import unittest
from unittest import mock

if __package__:
    from .selfhost_acceptance_support import (
        SelfhostAcceptanceTests,
        ProtectedMainWorkflowTrustTests,
        PublicMirrorSourceWorkflowTrustTests,
        ManualDispatchVerifierRevisionTests,
        ManualSelfhostReleaseWorkflowTests,
        SelfhostPathFilterPolicyTests,
        AcceptanceWorkflowTests,
        OverlappedStaticQueueTests,
        TEST_BASH,
        TEST_PATH,
        ROOT,
        HELPER,
        SPEC,
        acceptance,
        PROJECT,
        SERVICES,
        PUBLIC_SERVICES,
        CONTAINER_NAMES,
        binding,
        port_range,
        rendered_model,
        live_projection,
        public_rendered_model,
    )
else:
    from selfhost_acceptance_support import (
        SelfhostAcceptanceTests,
        ProtectedMainWorkflowTrustTests,
        PublicMirrorSourceWorkflowTrustTests,
        ManualDispatchVerifierRevisionTests,
        ManualSelfhostReleaseWorkflowTests,
        SelfhostPathFilterPolicyTests,
        AcceptanceWorkflowTests,
        OverlappedStaticQueueTests,
        TEST_BASH,
        TEST_PATH,
        ROOT,
        HELPER,
        SPEC,
        acceptance,
        PROJECT,
        SERVICES,
        PUBLIC_SERVICES,
        CONTAINER_NAMES,
        binding,
        port_range,
        rendered_model,
        live_projection,
        public_rendered_model,
    )


class _OwnedExecutor:
    """A tiny stateful model of shipped commands; it never starts host work."""

    def __init__(self, workspace: Path):
        self.workspace = workspace
        self.calls: list[list[str]] = []
        self.results: list[tuple[list[str], str | None, int, str, str]] = []
        self.events: list[tuple] = []
        self.stack = "absent"
        self.volumes_present = False
        self.env_exists = False
        self.log_boundary = False
        self.tls_ready = False
        self.row_present = False
        self.fail_startup = False
        self.preexisting_project = False
        self.malformed_guard: str | None = None
        self.lost_readback: str | None = None
        self.lifecycle_command_failure: tuple[str, int] | None = None
        self.required_shutdown_seconds = None
        self.shutdown_failure = None
        self.wrapper_timeouts = []
        self.inventory_variant = None
        self.inventory_after_refusal = False
        self.inventory_age = 1
        self.db_read_count = 0
        self.down_pending_readback = False
        self.down_restart_readback = False
        self.zero_lifecycle: str | None = None
        self.stopped_health_timeout = False
        self.refusal_result_override = None
        self.guard_count = {"log": 0, "tls": 0}
        self.failure_sentinel = "PRIVATE_CHILD_SENTINEL_2505"
        self.config_variants: list[tuple[tuple[str, ...], bool]] = []
        self.selected_model_validated = False
        self.sha_mismatch = False
        self.invalid_answer_zero = False
        self.valid_installer_result = None
        self.live_variant: str | None = None
        self.ops_agent_exit_code = 0
        self.daemon_endpoint = "unix:///var/run/docker.sock"
        self.engine_os = "linux"
        self.docker_version_result = self._result(stdout="29.8.2\n")
        self.daemon_cpus = 2
        self.capacity_model_mutation = None
        self.route_variant: str | None = None
        self.engineio_sid_counter = 0
        self.served_leaf_mismatch = False
        self.identity_project = PROJECT
        self.rendered_project = PROJECT
        self.live_project = PROJECT
        self.rendered_variant: str | None = None
        self.public_bundle = False
        self.public_extra_service: str | None = None
        self.log_sample_timeout_empty = False
        self.delivered_tls_metadata_variant: str | None = None
        self.nginx_active = False
        self.nginx_failure: str | None = None
        self.startup_stdout = ""
        self.startup_returncode = 37
        self.startup_phase = None
        self.startup_inspect_result = self._result(1, "", "owned diagnostic unavailable")
        self.startup_logs_result = self._result()
        # Installer output is an explicitly untracked fixture even when a new
        # executor is created before reset_install_state removes the prior env.
        self.tracked_paths = sorted(str(path.relative_to(workspace)) for path in workspace.rglob("*")
                                    if path.is_file() and path != workspace / ".env")
        self.git_dirty = False
        self.source_mutation = None
        self.log_sample_result = None
        self.invalid_logs_diagnostic = "selfhost-storage: unknown log service"

    @staticmethod
    def _result(returncode: int = 0, stdout: str = "", stderr: str = ""):
        # Construct the public helper contract's result type so the runner is
        # exercised through the same boundary as a real subprocess executor.
        return acceptance.CommandResult(returncode, stdout, stderr)

    def docker_inspect_fixture(self) -> list[dict]:
        """Convert the owned live projection to a bounded Docker inspect shape.

        Config.Env and all other unneeded inspect fields are deliberately absent;
        coturn fields match the source's selective selfhost-tls.py template.
        """
        result = []
        model = (public_rendered_model(self.live_project) if self.public_bundle
                 else rendered_model(self.live_project))
        projection = live_projection(model)
        if self.live_variant == "missing-control-plane":
            control = next(item for item in projection if item["service"] == "control-plane")
            control["ports"] = [p for p in control["ports"] if p["target"] != 8080]
        for item in projection:
            ports: dict[str, list[dict]] = {}
            for port in item["ports"]:
                key = f"{port['target']}/{port['protocol']}"
                ports.setdefault(key, []).append({
                    "HostIp": port["host_ip"], "HostPort": str(port["published"]),
                })
            result.append({
                "Id": item["id"], "Name": item["name"],
                "Config": {
                    "Image": item["image"], "User": "nobody:nogroup",
                    "Labels": {
                        "com.docker.compose.project": item["project"],
                        "com.docker.compose.service": item["service"],
                    },
                    "Cmd": (["turnserver", "-c", "/etc/coturn/turnserver.conf",
                             "--cert", "/etc/coturn/certs/cert.pem",
                             "--pkey", "/etc/coturn/certs/key.pem"]
                            if item["service"] == "coturn" else []),
                },
                "State": {
                    "Status": item["state"], "Running": item["state"] == "running",
                    "Paused": False, "Restarting": False,
                    "Health": (None if item["service"] in {"minio", "ops-agent"}
                               else {"Status": item["health"]}),
                    **({"ExitCode": self.ops_agent_exit_code}
                       if item["service"] == "ops-agent" else {}),
                },
                "Mounts": [{"Type": mount["type"], "Source": mount["source"],
                            "Destination": mount["target"], "RW": not mount["read_only"]}
                           for mount in item["mounts"]],
                "NetworkSettings": {"Ports": ports},
            })
        return result

    def run(self, argv: list[str], timeout: int, input_text: str | None = None):
        captured_argv = list(argv)
        self.calls.append(captured_argv)
        if argv[:3] == ["sudo", "-n", "/usr/bin/python3"]:
            if argv[3:5] != ["-I", "-c"] or argv[5] != acceptance._PRIVILEGED_SUPERVISOR:
                raise AssertionError("privileged supervision envelope is malformed")
            if not math.isfinite(float(argv[6])):
                raise AssertionError("privileged supervisor requires a finite absolute deadline")
            argv = ["sudo", "-n", *argv[7:]]
        command = " ".join(argv)
        operation = command + " " + (input_text or "")

        def finish(result):
            if self.refusal_result_override is not None and self.events:
                event_name, replacement = self.refusal_result_override
                matching_child = ((event_name.startswith("installer-") and "install-selfhost.sh" in command) or
                                  (not event_name.startswith("installer-") and
                                   argv[:2] == ["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost")]))
                if event_name == "wrapper-logs":
                    matching_child = matching_child and "not-a-service" in argv
                if matching_child and self.events[-1][0] == event_name:
                    result = replacement
            self.results.append((captured_argv, input_text, result.returncode, result.stdout, result.stderr))
            return result

        # Each recognized invocation has a source-derived answer. An unknown
        # child command is a fixture defect and must be visible to the author.
        if "docker" in argv and "version" in argv and "--format" in argv:
            return finish(self.docker_version_result)
        if "docker" in argv and "info" in argv:
            return finish(self._result(stdout=json.dumps({"OSType": self.engine_os, "ServerVersion": "29.8.2", "NCPU": self.daemon_cpus}) + "\n"))
        if "docker" in argv and "context" in argv and "inspect" in argv:
            return finish(self._result(stdout=self.daemon_endpoint + "\n"))
        if argv and Path(argv[0]).name == "git" and "rev-parse" in argv and "HEAD" in argv:
            sha = "b" * 40 if self.sha_mismatch else os.environ.get("GITHUB_SHA", "")
            return finish(self._result(stdout=sha + "\n"))
        if argv and Path(argv[0]).name == "git" and "ls-files" in argv:
            return finish(self._result(stdout="\0".join(self.tracked_paths) + "\0"))
        if argv and Path(argv[0]).name == "git" and "diff" in argv:
            return finish(self._result(1 if self.git_dirty else 0))
        if "docker" in argv and "compose" in argv and "version" in argv:
            return finish(self._result(stdout="2.24.5\n"))
        identity_argv = ["docker", "compose", "--project-directory", str(self.workspace),
                         "--env-file", "/dev/null", "-f", "-", "config", "--format", "json"]
        if argv == identity_argv:
            if input_text != "services:\n  identity:\n    image: scratch\n":
                raise AssertionError("project identity probe stdin differed from shipped source contract")
            self.events.append(("project-identity", self.identity_project, tuple(argv), input_text))
            return finish(self._result(stdout=json.dumps({
                "name": self.identity_project, "services": {"identity": {"image": "scratch"}}
            }) + "\n"))
        if "nproc" in argv:
            return finish(self._result(stdout="4\n"))
        if "df" in argv:
            return finish(self._result(stdout="Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/test 30000000 1000000 29000000 4% /\n"))
        if "free" in argv:
            return finish(self._result(stdout="Mem: 8192000 total 4096000 used 4096000 free\n"))
        if argv[0] in {"cat", "/bin/cat"} and argv[-1] == "/etc/os-release":
            return finish(self._result(stdout='ID=ubuntu\nVERSION_ID="24.04"\n'))
        if argv[0] in {"cat", "/bin/cat"} and argv[-1] == "/proc/meminfo":
            return finish(self._result(stdout="MemTotal: 8192000 kB\n"))
        if "logrotate" in argv:
            return finish(self._result(stdout="logrotate 3.21.0\n"))

        if "install-selfhost.sh" in command:
            if self.env_exists or (self.workspace / ".env").exists():
                env_file = self.workspace / ".env"
                before_bytes = env_file.read_bytes() if env_file.exists() else None
                before_mode = env_file.stat().st_mode & 0o777 if env_file.exists() else None
                result = self._result(1, "", "selfhost-storage: existing installation refused")
                after_bytes = env_file.read_bytes() if env_file.exists() else None
                after_mode = env_file.stat().st_mode & 0o777 if env_file.exists() else None
                self.events.append(("installer-rerun", before_bytes, before_mode,
                                    after_bytes, after_mode))
                return finish(result)
            answer_path = None
            if "--env-file" in argv:
                position = argv.index("--env-file")
                if position + 1 < len(argv):
                    answer_path = Path(argv[position + 1])
            answers = ""
            if answer_path is not None:
                try:
                    owned_answer = answer_path.resolve(strict=True)
                    owned_answer.relative_to(self.workspace.resolve())
                    answers = owned_answer.read_text(encoding="utf-8")
                except (OSError, ValueError):
                    return finish(self._result(1, "", "installer: answers file unavailable"))
            answer_values = dict(line.split("=", 1) for line in answers.splitlines()
                                 if "=" in line and not line.lstrip().startswith("#"))
            domain = answer_values.get("CFG_DOMAIN", "")
            domain_valid = bool(re.fullmatch(
                r"([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}", domain
            )) and not domain.startswith(("media.", "api.", "turn."))
            if not domain_valid:
                self.events.append(("installer-invalid", str((self.workspace / ".env").exists())))
                code = 0 if self.invalid_answer_zero else 1
                return finish(self._result(code, "" if code else "installer: configuration written",
                                           "" if not code else "installer: invalid domain"))
            public_ip = answer_values.get("CFG_PUBLIC_IP", "")
            if public_ip:
                try:
                    ipaddress.ip_address(public_ip)
                except ValueError:
                    self.events.append(("installer-invalid", str((self.workspace / ".env").exists())))
                    return finish(self._result(1, "", "invalid public IP: expected an IPv4 or IPv6 address"))
            if self.valid_installer_result is not None:
                self.events.append(("installer-failure", self.valid_installer_result.returncode))
                return finish(self.valid_installer_result)
            self.env_exists = True
            if self.source_mutation is not None:
                self.source_mutation(self)
            (self.workspace / ".env").write_text(
                "DOMAIN=ci-selfhost.example.test\nPUBLIC_IP=127.0.0.1\n"
                "SELFHOST_STORAGE_MODE=bundled\n",
                encoding="utf-8",
            )
            (self.workspace / ".env").chmod(0o600)
            # Mirror the shipped install-selfhost.sh renderer: it substitutes
            # the validated domain into the checked-in nginx source template.
            nginx_source = self.workspace / "infrastructure/deploy/nginx/concordvoice.conf"
            if not nginx_source.is_file():
                raise AssertionError("installer fixture lacks shipped nginx source template")
            rendered = nginx_source.read_text(encoding="utf-8").replace(
                "concordvoice.chat", domain)
            nginx_output = self.workspace / "infrastructure/deploy/nginx/concordvoice.rendered.conf"
            nginx_output.write_text(rendered, encoding="utf-8")
            published_bytes = (self.workspace / ".env").read_bytes()
            published_mode = (self.workspace / ".env").stat().st_mode & 0o777
            self.events.append(("installer-valid", published_bytes, published_mode))
            return finish(self._result(stdout="installer: configuration written"))
        if "provision-cert.sh" in command:
            self.tls_ready = True
            self.events.append(("provision-tls", "local"))
            return finish(self._result(stdout="local certificate provisioned"))
        if (("provision-production.sh" in command and "security-event-logging" in command)
                or "selfhost-security-event-logging.sh" in command):
            self.log_boundary = True
            self.events.append(("provision-log", "security-event-logging"))
            return finish(self._result(stdout="security-event-logging: prepared"))
        if "docker" in argv and "compose" in argv and "config" in argv:
            if "-p" not in argv or argv[argv.index("-p") + 1] != self.identity_project:
                raise AssertionError(f"selected model did not use derived project identity: {argv!r}")
            file_args = [Path(argv[index + 1]) for index, token in enumerate(argv[:-1])
                         if token == "-f"]
            files = tuple(path.name for path in file_args)
            expected_paths = (self.workspace / "docker-compose.yml",
                              self.workspace / "docker-compose.production.yml",
                              self.workspace / "docker-compose.selfhost.yml")
            exact_owned_paths = (len(file_args) in (2, 3) and
                                 all((path if path.is_absolute() else self.workspace / path)
                                     .resolve(strict=False) == expected.resolve()
                                     for path, expected in zip(file_args, expected_paths)))
            quiet = "--quiet" in argv or "-q" in argv
            profile_ok = "--profile" in argv and argv[argv.index("--profile") + 1] == "services"
            base_pair = files == ("docker-compose.yml", "docker-compose.production.yml")
            selected = files == ("docker-compose.yml", "docker-compose.production.yml",
                                 "docker-compose.selfhost.yml")
            valid = exact_owned_paths and ((base_pair and quiet) or
                                           (selected and not quiet and profile_ok))
            self.config_variants.append((files, valid))
            self.events.append(("compose-config", files, quiet, profile_ok, valid,
                                exact_owned_paths))
            if not valid:
                return finish(self._result(1, "", "invalid Compose model selection"))
            if quiet:
                return finish(self._result())
            self.selected_model_validated = True
            model = (public_rendered_model(self.rendered_project) if self.public_bundle
                     else rendered_model(self.rendered_project))
            if self.capacity_model_mutation is not None:
                self.capacity_model_mutation(model)
            self.events.append(("selected-model", self.identity_project, model["name"]))
            if self.rendered_variant == "model-invalid":
                return finish(self._result(stdout="[]\n"))
            if self.rendered_variant == "service-invalid":
                model["services"]["postgres"] = None
            return finish(self._result(stdout=json.dumps(model)))
        if argv[:2] == ["docker", "logs"]:
            if argv != ["docker", "logs", "--tail=20", "concordvoice-media-plane"] or timeout != 8:
                raise AssertionError("startup logs must be a bounded read of the selected media container")
            self.events.append(("startup-diagnostic", "logs"))
            if isinstance(self.startup_logs_result, Exception):
                raise self.startup_logs_result
            return finish(self.startup_logs_result)
        if (argv[:2] == ["docker", "inspect"] and
                argv[-1:] == ["concordvoice-media-plane"] and
                "--format" in argv and "state" in argv[argv.index("--format") + 1]):
            if timeout != 8 or argv[2:4] != ["--type", "container"]:
                raise AssertionError("startup inspect must be a bounded container-only read")
            self.events.append(("startup-diagnostic", "inspect"))
            if isinstance(self.startup_inspect_result, Exception):
                raise self.startup_inspect_result
            return finish(self.startup_inspect_result)
        if "docker" in argv and ("ps" in argv or "volume" in argv or "inspect" in argv):
            filter_values = [argv[index + 1] for index, token in enumerate(argv[:-1])
                             if token == "--filter"]
            if "ps" in argv or "volume" in argv:
                if f"label=com.docker.compose.project={self.identity_project}" not in filter_values:
                    raise AssertionError(f"project state scan did not use derived project identity: {argv!r}")
                self.events.append(("project-state-scan",
                                    "containers" if "ps" in argv else "volumes",
                                    self.identity_project, tuple(filter_values)))
            if "volume" in argv:
                has_volumes = self.preexisting_project or self.volumes_present
                value = "acceptance-owned-postgres\nacceptance-owned-minio\n" if has_volumes else ""
                if self.inventory_after_refusal and value:
                    if self.inventory_variant == "benign":
                        value = "acceptance-owned-minio\nacceptance-owned-postgres\n"
                    elif self.inventory_variant == "volume-add":
                        value += "acceptance-owned-extra\n"
                    elif self.inventory_variant == "volume-delete":
                        value = "acceptance-owned-postgres\n"
            elif "inspect" in argv:
                if self.public_bundle and "concordvoice-ops-agent" in argv:
                    raise AssertionError("public acceptance must not inspect an inactive managed agent")
                # Docker inspect of the fixed Compose names fails before first
                # creation. Preserve Docker's exact bounded absence signal;
                # an empty successful JSON array is a different condition.
                if self.stack == "absent" and "--format" in argv:
                    missing = "\n".join(
                        f"Error response from daemon: No such container: {CONTAINER_NAMES[service]}"
                        for service in sorted(PUBLIC_SERVICES if self.public_bundle else SERVICES))
                    return finish(self._result(1, "", missing))
                inspected_rows = self.docker_inspect_fixture() if self.stack == "running" else []
                if self.inventory_after_refusal and self.inventory_variant == "health-change":
                    for row in inspected_rows:
                        if row["Config"]["Labels"]["com.docker.compose.service"] == "coturn":
                            row["State"]["Health"] = {"Status": "unhealthy"}
                value = json.dumps(inspected_rows)
            elif "--format" in argv:
                if self.inventory_after_refusal and self.inventory_variant == "benign":
                    self.inventory_age += 1
                has_project = self.preexisting_project or self.stack in {"running", "stopped"}
                if self.preexisting_project:
                    services = ("postgres",)
                elif has_project:
                    services = tuple(sorted(PUBLIC_SERVICES if self.public_bundle else SERVICES))
                    if self.public_extra_service is not None:
                        services += (self.public_extra_service,)
                else:
                    services = ()
                rows = [{"ID": f"owned-{service}-id", "Names": f"concordvoice-{service}",
                         "State": ("exited" if service == "ops-agent" else
                                   "exited" if self.stack == "stopped" else "running"),
                         "Status": f"Up {self.inventory_age} seconds",
                         "RunningFor": f"{self.inventory_age} seconds ago",
                         "Labels": f"com.docker.compose.project={self.identity_project},com.docker.compose.service={service}"}
                        for service in services]
                if self.inventory_after_refusal and rows:
                    if self.inventory_variant == "benign":
                        rows.reverse()
                    elif self.inventory_variant == "id-change":
                        rows[0]["ID"] = "owned-replacement-id"
                    elif self.inventory_variant == "state-change":
                        rows[0]["State"] = "exited"
                    elif self.inventory_variant == "duplicate-id":
                        rows[1]["ID"] = rows[0]["ID"]
                    elif self.inventory_variant == "invalid-id":
                        rows[0]["ID"] = "PRIVATE INVALID ID"
                    elif self.inventory_variant == "invalid-state":
                        rows[0]["State"] = "PRIVATE_INVALID_STATE"
                    elif self.inventory_variant == "invalid-json":
                        return finish(self._result(stdout="{PRIVATE_INVALID_JSON"))
                format_arg = argv[argv.index("--format") + 1] if argv.index("--format") + 1 < len(argv) else ""
                if format_arg.strip() == "json" or "{{json .}}" in format_arg:
                    # Docker's JSON formatter streams one object per container;
                    # an empty project therefore produces empty stdout, not [].
                    value = "\n".join(json.dumps(row) for row in rows)
                else:
                    value = "owned-container-id\n" if rows else ""
            else:
                value = "owned-existing-container\n" if self.preexisting_project else ""
            return finish(self._result(stdout=value))
        if argv[:2] == ["bash", str(self.workspace / "infrastructure/deploy/concord-selfhost")]:
            verb = next((part for part in argv if part in
                         {"up", "health", "status", "logs", "stop", "down", "unknown"}), "")
            self.wrapper_timeouts.append((verb, timeout))
            if verb in {"stop", "down"}:
                if self.shutdown_failure is not None and self.shutdown_failure[0] == verb:
                    return finish(self._result(self.shutdown_failure[1], "", self.failure_sentinel))
                if self.required_shutdown_seconds is not None and timeout < self.required_shutdown_seconds:
                    return finish(self._result(124, "", "owned shutdown exceeded caller budget"))
            successful_count = sum(event == ("wrapper", verb) for event in self.events)
            if (self.log_boundary and self.tls_ready and
                    self.lifecycle_command_failure == (verb, successful_count + 1)):
                self.events.append(("lifecycle-command-failure", verb))
                return finish(self._result(43, "", self.failure_sentinel))
            if verb == "up":
                if not self.selected_model_validated:
                    return finish(self._result(1, "", "selected Compose model was not validated"))
                if not self.log_boundary:
                    self.guard_count["log"] += 1
                    before = self.stack
                    code = 0 if self.malformed_guard == "log-zero" else 1
                    reason = ("wrong refusal" if self.malformed_guard == "log-wrong"
                              else "security-event log boundary missing: /var/log/concord-security")
                    if self.malformed_guard == "log-mutate":
                        self.stack = "running"
                    self.events.append(("guard-log", str(code), reason, before, self.stack))
                    return finish(self._result(code, "", reason))
                if not self.tls_ready:
                    self.guard_count["tls"] += 1
                    before = self.stack
                    code = 0 if self.malformed_guard == "tls-zero" else 1
                    reason = "wrong refusal" if self.malformed_guard == "tls-wrong" else "self-host TLS material refused"
                    if self.malformed_guard == "tls-mutate":
                        self.stack = "running"
                    self.events.append(("guard-tls", str(code), reason, before, self.stack))
                    return finish(self._result(code, "", reason))
                if self.fail_startup:
                    self.events.append(("startup-failure", str(self.startup_returncode)))
                    if self.startup_phase is not None:
                        return finish(SimpleNamespace(returncode=self.startup_returncode,
                                                      stdout=self.startup_stdout, stderr=self.failure_sentinel,
                                                      startup_phase=self.startup_phase))
                    return finish(self._result(self.startup_returncode, self.startup_stdout,
                                               self.failure_sentinel))
                if self.lost_readback == "down" and self.down_pending_readback:
                    self.down_restart_readback = True
                self.stack = "running"
                self.volumes_present = True
            elif verb == "health":
                if self.stack != "running":
                    code = 0 if self.zero_lifecycle == "health-after-stop" else 1
                    if self.stopped_health_timeout:
                        code = 124 if self.stopped_health_timeout is True else self.stopped_health_timeout
                    result = self._result(code, "health passed" if code == 0 else "",
                                          "" if code == 0 else "✗ control-plane unhealthy (HTTP 000)" +
                                          ("\n" + self.failure_sentinel if self.stopped_health_timeout else ""))
                    self.events.append(("wrapper", verb))
                    return finish(result)
                self.events.append(("wrapper", verb))
                return finish(self._result(stdout="health passed"))
            elif verb == "status":
                self.events.append(("wrapper", verb))
                return finish(self._result(stdout=f"stack={self.stack}\n"))
            elif verb == "logs":
                if not any(part in SERVICES for part in argv):
                    code = 0 if self.zero_lifecycle == "logs" else 1
                    reason = self.invalid_logs_diagnostic
                    self.events.append(("wrapper-logs", str(code), reason))
                    self.inventory_after_refusal = True
                    return finish(self._result(code, reason if code == 0 else "",
                                               "" if code == 0 else reason))
                if self.log_sample_timeout_empty:
                    self.events.append(("wrapper-log-sample", "124", ""))
                    return finish(self._result(124))
                if self.log_sample_result is not None:
                    return finish(self.log_sample_result)
                self.events.append(("wrapper-logs", "0", "bounded service log sample"))
                return finish(self._result(stdout="bounded service log sample"))
            elif verb == "stop":
                self.stack = "stopped"
                if self.lost_readback == "stop":
                    self.row_present = False
            elif verb == "down":
                self.stack = "absent"
                if "--volumes" in argv or "-v" in argv:
                    self.volumes_present = False
                if self.lost_readback == "down":
                    self.down_pending_readback = True
            elif verb == "unknown":
                code = 0 if self.zero_lifecycle == "unknown" else 1
                reason = "unknown verb: unknown"
                self.events.append(("wrapper-unknown", str(code), reason))
                return finish(self._result(code, reason if code == 0 else "",
                                           "" if code == 0 else reason))
            else:
                raise AssertionError(f"fixture sent an unmodeled wrapper action: {argv!r}")
            self.events.append(("wrapper", verb))
            return finish(self._result(stdout=f"{verb} completed"))
        if "psql" in command and "INSERT" in operation.upper():
            self.row_present = True
            self.events.append(("db", "insert"))
            return finish(self._result(stdout="INSERT 0 1\n"))
        if "psql" in command and "SELECT" in operation.upper():
            value = "owned-row" if self.row_present else "missing"
            self.db_read_count += 1
            # Keep the pre-restart baseline intact. This mutation models loss
            # only when a post-restart readback is requested.
            if (self.lost_readback == "stop" and self.db_read_count > 1
                    or self.lost_readback == "down" and self.down_restart_readback):
                value = "missing"
            self.events.append(("db-read", value))
            tuple_only = any(flag in argv for flag in ("-t", "-tA", "-At"))
            unaligned = any(flag in argv for flag in ("-A", "-tA", "-At"))
            if tuple_only and unaligned:
                stdout = value + "\n"
            else:
                stdout = ("   id\n---------\n" + value + "\n(1 row)\n")
            return finish(self._result(stdout=stdout))
        if "curl" in argv:
            url = next((part for part in argv if part.startswith(("http://", "https://"))), "")
            if "--http1.1" not in argv:
                raise AssertionError(f"route probe must explicitly use HTTP/1.1: {argv!r}")
            if url.startswith("https://"):
                host = url.split("/", 3)[2]
                if ("--resolve" not in argv or
                        argv[argv.index("--resolve") + 1] != f"{host}:443:127.0.0.1" or
                        "--cacert" not in argv or
                        argv[argv.index("--cacert") + 1] !=
                        "/etc/letsencrypt/live/ci-selfhost.example.test/fullchain.pem"):
                    raise AssertionError(f"HTTPS route is not pinned to the local trust anchor: {argv!r}")
            routes = {
                "http://127.0.0.1:8080/health":
                    ("control-local", '{"status":"healthy","service":"control-plane"}'),
                "http://127.0.0.1:8080/readyz":
                    ("control-ready", '{"status":"ready","service":"control-plane","checks":[]}'),
                "http://127.0.0.1:3000/health":
                    ("media-local", '{"status":"healthy","service":"media-plane","workers":1,"activeRooms":0,"totalParticipants":0,"totalProducers":0,"totalConsumers":0,"metrics":{}}'),
                "http://127.0.0.1:3000/readyz":
                    ("media-ready", '{"ready":true,"voiceEnforcementProtocol":3}'),
                "https://api.ci-selfhost.example.test/health":
                    ("nginx-api", '{"status":"healthy","service":"control-plane"}'),
                "https://api.ci-selfhost.example.test/nginx-health": ("nginx-owned", "ok"),
            }
            if url == "https://media.ci-selfhost.example.test/socket.io/?EIO=4&transport=polling":
                self.engineio_sid_counter += 1
                packet = {"sid": f"fixture-owned-session-{self.engineio_sid_counter}",
                          "upgrades": ["websocket"], "pingInterval": 25000,
                          "pingTimeout": 20000, "maxPayload": 1000000}
                routes[url] = ("nginx-media", "0" + json.dumps(packet, separators=(",", ":")))
            if url not in routes:
                raise AssertionError(f"fake executor has no route-specific response for {argv!r}")
            route, body = routes[url]
            if self.route_variant == route:
                body = "wrong-owned-route-response"
            self.events.append(("route", route, url))
            return finish(self._result(stdout="HTTP/1.1 200 OK\n" + body + "\n"))
        if argv and argv[0] == "sudo" and argv[1:] and argv[1] == "-n":
            if len(argv) == 11 and argv[2:7] == ["install", "-o", "root", "-g", "root"]:
                source = Path(argv[9])
                if (argv[7:9] != ["-m", "0644"]
                        or argv[10] != "/etc/nginx/conf.d/concordvoice.conf"
                        or source != self.workspace / "infrastructure/deploy/nginx/concordvoice.rendered.conf"
                        or not source.is_file()):
                    raise AssertionError(f"unexpected nginx vhost installation: {argv!r}")
                template = self.workspace / "infrastructure/deploy/nginx/concordvoice.conf"
                expected = template.read_text(encoding="utf-8").replace(
                    "concordvoice.chat", "ci-selfhost.example.test")
                if source.read_text(encoding="utf-8") != expected:
                    raise AssertionError("nginx vhost is not the installer's source-derived rendering")
                self.events.append(("nginx-vhost-install", tuple(argv)))
                if self.nginx_failure == "nginx_vhost":
                    return finish(self._result(1, "", "owned installation failure"))
                return finish(self._result())
            if argv[2:3] == ["nginx"]:
                if argv[2:] != ["nginx", "-t", "-c", "/etc/nginx/concord-selfhost-ci.conf"]:
                    raise AssertionError("nginx syntax must use isolated /etc/nginx/concord-selfhost-ci.conf")
                self.events.append(("nginx-config-test", tuple(argv)))
                if self.nginx_failure == "nginx_config":
                    return finish(self._result(1, "", "owned configuration failure"))
                return finish(self._result(stdout="nginx: configuration is valid\n"))
            if argv[2:] == ["systemctl", "enable", "nginx"]:
                self.events.append(("nginx-enable", tuple(argv)))
                if self.nginx_failure == "nginx_service_enable":
                    return finish(self._result(41, "PRIVATE_NGINX_STDOUT", "PRIVATE_NGINX_ENV=opaque sudo systemctl enable nginx"))
                return finish(self._result())
            if argv[2:] == ["systemctl", "start", "nginx"]:
                self.events.append(("nginx-start", tuple(argv)))
                if self.nginx_failure == "nginx_service_start":
                    return finish(self._result(42, "PRIVATE_NGINX_STDOUT", "PRIVATE_NGINX_ENV=opaque sudo systemctl start nginx"))
                self.nginx_active = True
                return finish(self._result())
            if argv[2:] == ["systemctl", "is-active", "--quiet", "nginx"]:
                self.events.append(("nginx-active", tuple(argv)))
                if self.nginx_failure == "nginx_service_active":
                    return finish(self._result(43, "PRIVATE_NGINX_STDOUT", "PRIVATE_NGINX_ENV=opaque sudo systemctl is-active --quiet nginx"))
                return finish(self._result(0 if self.nginx_active else 3))
            if argv[2:] == ["systemctl", "reload", "nginx"]:
                self.events.append(("nginx-reload", tuple(argv)))
                if self.nginx_failure == "nginx_service_reload":
                    return finish(self._result(44, "PRIVATE_NGINX_STDOUT", "PRIVATE_NGINX_ENV=opaque sudo systemctl reload nginx"))
                return finish(self._result(0 if self.nginx_active else 1))
            raise AssertionError(f"fake executor has no source-backed response for argv: {argv!r}")
        if "openssl" in argv and "s_client" in argv:
            required_probe = ("-connect" in argv and
                              argv[argv.index("-connect") + 1] == "127.0.0.1:5349" and
                              "-servername" in argv and
                              argv[argv.index("-servername") + 1] == "turn.ci-selfhost.example.test" and
                              "-verify_hostname" in argv and
                              argv[argv.index("-verify_hostname") + 1] == "turn.ci-selfhost.example.test" and
                              "-CAfile" in argv and
                              argv[argv.index("-CAfile") + 1] == "/etc/letsencrypt/live/ci-selfhost.example.test/fullchain.pem")
            if not required_probe:
                raise AssertionError(f"TLS probe did not target the owned local TURN endpoint: {argv!r}")
            self.events.append(("served-leaf-probe", tuple(argv)))
            return finish(self._result(stdout="SYNTHETIC_SERVED_CERTIFICATE_2505\n"))
        if "openssl" in argv and "x509" in argv and "-fingerprint" in argv:
            is_served = input_text == "SYNTHETIC_SERVED_CERTIFICATE_2505\n"
            if is_served:
                target = "served"
            elif ("-in" in argv and argv[argv.index("-in") + 1] ==
                  "/etc/letsencrypt/live/ci-selfhost.example.test/fullchain.pem"):
                target = "source"
            else:
                raise AssertionError(f"x509 fingerprint must identify the selected local source or probe input: {argv!r}")
            fingerprint = ("BB:BB:BB:BB" if self.served_leaf_mismatch else "AA:AA:AA:AA") if target == "served" else "AA:AA:AA:AA"
            self.events.append(("leaf-fingerprint", target, fingerprint, tuple(argv), input_text))
            return finish(self._result(stdout=f"sha256 Fingerprint={fingerprint}\n"))
        if argv == ["stat", "-c", "%F|%u|%g|%a|%s",
                    "/opt/concord/certs", "/opt/concord/certs/coturn",
                    "/opt/concord/certs/coturn/cert.pem",
                    "/opt/concord/certs/coturn/key.pem"]:
            root = "directory|0|0|711|4096"
            coturn = "directory|0|0|711|4096"
            cert = "regular file|0|0|444|1024"
            key = "regular file|65534|65533|400|2048"
            if self.delivered_tls_metadata_variant == "root-owner":
                root = "directory|1000|0|711|4096"
            elif self.delivered_tls_metadata_variant == "root-mode":
                root = "directory|0|0|755|4096"
            elif self.delivered_tls_metadata_variant == "root-not-directory":
                root = "regular file|0|0|711|4096"
            elif self.delivered_tls_metadata_variant == "root-empty":
                root = "directory|0|0|711|0"
            elif self.delivered_tls_metadata_variant == "coturn-owner":
                coturn = "directory|0|1000|711|4096"
            elif self.delivered_tls_metadata_variant == "cert-owner":
                cert = "regular file|1000|0|444|1024"
            elif self.delivered_tls_metadata_variant == "cert-mode":
                cert = "regular file|0|0|644|1024"
            elif self.delivered_tls_metadata_variant == "key-owner":
                key = "regular file|0|0|400|2048"
            elif self.delivered_tls_metadata_variant == "key-mode":
                key = "regular file|65534|65533|600|2048"
            elif self.delivered_tls_metadata_variant == "not-regular":
                cert = "symbolic link|0|0|444|1024"
            elif self.delivered_tls_metadata_variant == "empty":
                key = "regular file|65534|65533|400|0"
            elif self.delivered_tls_metadata_variant == "malformed":
                cert = "unexpected stat output"
            value = "\n".join((root, coturn, cert, key)) + "\n"
            self.events.append(("delivered-tls-metadata", self.delivered_tls_metadata_variant,
                                tuple(argv), value))
            return finish(self._result(stdout=value))
        raise AssertionError(f"fake executor has no source-backed response for argv: {argv!r}")



class AcceptanceRunnerTests(unittest.TestCase):
    BLACKSMITH_PROFILE = {"RUNNER_ENVIRONMENT": "self-hosted",
                          "SELFHOST_ACCEPTANCE_RUNNER_PROFILE": "blacksmith-4vcpu-ubuntu-2404",
                          "GITHUB_REPOSITORY": "Concord-Voice/Concord-Voice-Alpha"}
    HOSTED = {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted",
              "RUNNER_OS": "Linux", "GITHUB_EVENT_NAME": "pull_request",
              "GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "run-2505"}

    def setUp(self):
        # Runner fixtures model hosted Ubuntu, independently of the test host.
        host_platform_patch = mock.patch.object(acceptance.platform, "system", return_value="Linux")
        host_platform_patch.start()
        self.addCleanup(host_platform_patch.stop)
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.evidence_scenario = 0
        # The runner receives an owned checkout-shaped workspace. Source files
        # are copied as inert fixtures and are never executed by this fake.
        for relative in ("docker-compose.yml", "docker-compose.production.yml",
                         "docker-compose.selfhost.yml", "infrastructure/deploy/install-selfhost.sh",
                         "infrastructure/deploy/concord-selfhost",
                         "infrastructure/deploy/concord-ctl.sh",
                         "infrastructure/deploy/provision-cert.sh",
                         "infrastructure/deploy/provision-production.sh",
                         "infrastructure/deploy/nginx/concordvoice.conf",
                         "infrastructure/deploy/selfhost-storage.sh",
                         "infrastructure/deploy/selfhost-tls.sh",
                         "infrastructure/deploy/selfhost-tls.py",
                         "services/media-plane/Dockerfile"):
            destination = self.workspace / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes((ROOT / relative).read_bytes())
        self.executor = _OwnedExecutor(self.workspace)

    def test_subprocess_executor_does_not_inject_ambient_compose_project(self):
        program = "import json,os,sys; print(json.dumps({'input':sys.stdin.read(),'compose':os.getenv('COMPOSE_PROJECT_NAME'),'cwd':os.getcwd()}))"
        with mock.patch.dict(os.environ, {"COMPOSE_PROJECT_NAME": "ambient-foreign-project"}):
            result = acceptance.SubprocessExecutor(self.workspace).run(
                [sys.executable, "-c", program], timeout=5, input_text="owned input")
        self.assertEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout), {"input": "owned input", "compose": None,
                                                    "cwd": str(self.workspace)})

    def test_real_docker_version_subprocess_preserves_invalid_bytes_and_runner_refuses(self):
        stub_bin = self.root / "docker-version-bin"
        stub_bin.mkdir()
        docker = stub_bin / "docker"
        for payload in (b"29.8.2\r\n", b"29.8.2\x00\n"):
            docker.write_text(
                "#!" + sys.executable + "\nimport sys\n"
                "if sys.argv[1:] != ['version', '--format', '{{.Server.Version}}']: "
                "raise SystemExit(97)\n"
                "sys.stdout.buffer.write(" + repr(payload) + ")\n")
            docker.chmod(0o700)
            with mock.patch.dict(os.environ, {"PATH": str(stub_bin) + os.pathsep + TEST_PATH}):
                result = acceptance.SubprocessExecutor(self.workspace).run(
                    ["docker", "version", "--format", "{{.Server.Version}}"], timeout=5)
            self.assertEqual(result.returncode, 0)
            with self.subTest(payload=payload, boundary="subprocess capture"):
                self.assertEqual(result.stdout, payload.decode("ascii"),
                                 "Docker version capture must retain bytes needed for strict admission")
            with self.subTest(payload=payload, boundary="runner admission"):
                self.reset_install_state()
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.docker_version_result = result
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "TOOL_VERSION_INVALID")
                self.assert_failure_evidence("TOOL_VERSION_INVALID")
                self.assert_no_mutations()

    def test_installer_strict_shell_refuses_nul_status_forgery_from_failed_docker_query(self):
        stub_bin = self.root / "installer-docker-bin"
        stub_bin.mkdir()
        docker = stub_bin / "docker"
        cases = ((b"29.8.2\n", 0, True),
                 (b"29.8.2\x000\x00", 42, False),
                 (b"29.8.2\x000\x00", 0, False))
        for payload, query_status, admitted in cases:
            with self.subTest(payload=payload, query_status=query_status):
                docker.write_text(
                    "#!" + sys.executable + "\nimport sys\n"
                    "args = sys.argv[1:]\n"
                    "if args in (['compose', 'version'], ['compose', 'version', '--short']):\n"
                    " print('2.29.1'); raise SystemExit(0)\n"
                    "if args != ['version', '--format', '{{.Server.Version}}']: raise SystemExit(97)\n"
                    "sys.stdout.buffer.write(" + repr(payload) + ")\n"
                    "raise SystemExit(" + repr(query_status) + ")\n")
                docker.chmod(0o700)
                # The shell executes the guard as a plain command: wrapping it in
                # an if/|| context would disable the errexit defect under test.
                result = subprocess.run(
                    [TEST_BASH, "-e", "-c", 'source "$1"; preflight_docker; printf "INSTALLER_ADMITTED\\n"',
                     "installer-strict-query", str(ROOT / "infrastructure/deploy/install-selfhost.sh")],
                    cwd=self.workspace, env={"PATH": str(stub_bin) + os.pathsep + TEST_PATH},
                    text=True, capture_output=True, timeout=10)
                if admitted:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("INSTALLER_ADMITTED", result.stdout)
                else:
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertNotIn("INSTALLER_ADMITTED", result.stdout)
                    self.assertNotIn("✓ docker", result.stdout)

    def test_executor_timeout_stops_owned_grandchild_activity(self):
        activity = self.root / "owned-child-activity"
        pid_file = self.root / "owned-child-pid"
        child = ("import pathlib,time; p=pathlib.Path(" + repr(str(activity)) + "); "
                 "\nwhile True:\n with p.open('a') as out: out.write('owned\\n'); out.flush()\n time.sleep(0.02)\n")
        parent = ("import pathlib,subprocess,sys,time; "
                  "p=subprocess.Popen([sys.executable,'-c'," + repr(child) + "],"
                  "stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL); "
                  "pathlib.Path(" + repr(str(pid_file)) + ").write_text(str(p.pid)); time.sleep(30)")
        try:
            result = acceptance.SubprocessExecutor(self.workspace).run([sys.executable, "-c", parent], timeout=1)
            self.assertEqual(result.returncode, 124)
            self.assertTrue(pid_file.is_file())
            self.assertTrue(activity.is_file(), "owned descendant must run before cancellation")
            before = activity.read_bytes()
            time.sleep(0.15)
            self.assertEqual(activity.read_bytes(), before, "no descendant activity after executor returns timeout")
        finally:
            if pid_file.is_file():
                try:
                    os.kill(int(pid_file.read_text()), signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def runner(self):
        return acceptance.AcceptanceRunner(self.executor, self.workspace, self.evidence)

    def test_privileged_foreground_commands_are_supervised_inside_sudo(self):
        clock = mock.Mock()
        clock.monotonic.return_value = 0.0
        with mock.patch.object(acceptance, "time", clock):
            result = self.run_admitted()
        privileged = [call for call in self.executor.calls if call[:2] == ["sudo", "-n"]]
        self.assertTrue(privileged)
        for call in privileged:
            self.assertEqual(call[:5], ["sudo", "-n", "/usr/bin/python3", "-I", "-c"])
            self.assertEqual(call[5], acceptance._PRIVILEGED_SUPERVISOR)
            self.assertEqual(float(call[6]), 30)
        self.assertTrue(any("provision-cert.sh" in " ".join(call[7:]) for call in privileged))
        self.assertTrue(any("provision-production.sh" in " ".join(call[7:]) for call in privileged))
        self.assertTrue(any(call[7] == "nginx" for call in privileged))
        self.assertTrue(any(call[7] == "systemctl" for call in privileged))
        self.assertTrue(all(check["status"] == "PASS" for check in result["checks"]))

    def test_privileged_supervision_clips_inner_duration_and_refuses_missing_reserve(self):
        for remaining, code in ((24, 0), (24, 17), (10, 0), (9, 0)):
            with self.subTest(remaining=remaining, code=code):
                executor = mock.Mock()
                executor.run.return_value = acceptance.CommandResult(code, "", "PRIVATE_ROOT_CHILD_OUTPUT")
                runner = acceptance.AcceptanceRunner(executor, self.workspace, self.evidence)
                runner.deadline = remaining
                clock = mock.Mock()
                clock.monotonic.return_value = 0
                command = ["sudo", "-n", "nginx", "-t"]
                with mock.patch.object(acceptance, "time", clock):
                    if code or remaining <= 10:
                        with self.assertRaises(acceptance.AcceptanceError) as caught:
                            runner._run(command, timeout=30)
                        self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                        self.assertEqual(caught.exception.returncode, 124 if remaining <= 10 else code)
                        self.assertNotIn("PRIVATE_ROOT_CHILD_OUTPUT", str(caught.exception))
                    else:
                        self.assertEqual(runner._run(command, timeout=30).returncode, 0)
                if remaining <= 10:
                    executor.run.assert_not_called()
                else:
                    call = executor.run.call_args
                    self.assertEqual(call.args[0][:5], ["sudo", "-n", "/usr/bin/python3", "-I", "-c"])
                    self.assertEqual(call.args[0][5], acceptance._PRIVILEGED_SUPERVISOR)
                    self.assertEqual(call.args[0][7:], ["nginx", "-t"])
                    self.assertEqual(float(call.args[0][6]), 24)
                    self.assertEqual(call.kwargs["timeout"], 24)

    def test_privileged_launcher_recomputes_budget_after_privilege_transition(self):
        command = ["nginx", "-t"]
        for deadline, current in (("24", 0), ("24", 8)):
            with self.subTest(deadline=deadline, current=current):
                with mock.patch.object(sys, "argv", ["owned-root-launcher", deadline, *command]), \
                     mock.patch.object(time, "monotonic", return_value=current), \
                     mock.patch.object(os, "execv") as execute:
                    exec(acceptance._PRIVILEGED_SUPERVISOR, {"__name__": "__main__"})  # nosemgrep
                execute.assert_called_once()
                binary, arguments = execute.call_args.args
                self.assertEqual(binary, "/usr/bin/timeout")
                self.assertEqual(arguments[:4], ["/usr/bin/timeout", "--signal=TERM", "--kill-after=5s", "--"])
                self.assertTrue(arguments[4].endswith("s"))
                self.assertEqual(float(arguments[4][:-1]), 14 - current)
                self.assertEqual(arguments[5:], command)

    def test_privileged_launcher_refuses_invalid_or_expired_budget_without_exec(self):
        for arguments, current in ((["24", "nginx", "-t"], 14),
                                   (["24", "nginx", "-t"], 15),
                                   (["nan", "nginx"], 0), (["inf", "nginx"], 0),
                                   (["invalid-private-value", "nginx"], 0),
                                   (["24"], 0), ([], 0)):
            with self.subTest(arguments=arguments, current=current):
                with mock.patch.object(sys, "argv", ["owned-root-launcher", *arguments]), \
                     mock.patch.object(time, "monotonic", return_value=current), \
                     mock.patch.object(os, "execv") as execute:
                    with self.assertRaises(SystemExit) as caught:
                        exec(acceptance._PRIVILEGED_SUPERVISOR, {"__name__": "__main__"})  # nosemgrep
                self.assertEqual(caught.exception.code, 124)
                execute.assert_not_called()

    def test_shared_run_deadline_clips_later_startup_and_rejects_late_completion(self):
        for final_code in (0, 37):
            with self.subTest(final_code=final_code):
                self.executor = _OwnedExecutor(self.workspace)
                self.reset_install_state()
                now = [0.0]
                startup_budgets = []
                original = self.executor.run
                clock = mock.Mock()
                clock.monotonic.side_effect = lambda: now[0]
                clock.sleep.side_effect = lambda seconds: now.__setitem__(0, now[0] + seconds)

                def child(argv, timeout, input_text=None):
                    self.assertLess(now[0], 2580, "no next command after shared deadline")
                    self.assertGreater(timeout, 0)
                    self.assertLessEqual(timeout, 2580 - now[0])
                    result = original(argv, timeout, input_text)
                    if ("concord-selfhost" in " ".join(argv) and "up" in argv and
                            self.executor.log_boundary and self.executor.tls_ready):
                        startup_budgets.append(timeout)
                        if len(startup_budgets) < 3:
                            now[0] += 900
                        else:
                            now[0] += timeout
                            return acceptance.CommandResult(final_code, "PRIVATE_LATE_COMPLETION", "")
                    return result

                with mock.patch.object(acceptance, "time", clock, create=True), \
                     mock.patch.object(self.executor, "run", side_effect=child):
                    with self.assertRaises(acceptance.AcceptanceError) as caught:
                        self.run_admitted()
                self.assertEqual(startup_budgets, [2580, 1680, 780])
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                self.assertEqual(caught.exception.returncode, 124 if final_code == 0 else final_code)
                record = self.assert_failure_evidence("COMMAND_FAILED")
                self.assertNotIn("PRIVATE_LATE_COMPLETION", json.dumps(record))
                self.assertFalse(any(check["assertion"] == "plain_down_preserves_volumes" and check["status"] == "PASS"
                                     for check in record["checks"]))

    def test_valid_caller_can_complete_after_35_minutes_within_shared_budget(self):
        now = [0.0]
        startup_budgets = []
        original = self.executor.run
        clock = mock.Mock()
        clock.monotonic.side_effect = lambda: now[0]
        clock.sleep.side_effect = lambda seconds: now.__setitem__(0, now[0] + seconds)

        def child(argv, timeout, input_text=None):
            self.assertLess(now[0], 2580)
            self.assertLessEqual(timeout, 2580 - now[0])
            result = original(argv, timeout, input_text)
            if ("concord-selfhost" in " ".join(argv) and "up" in argv and
                    self.executor.log_boundary and self.executor.tls_ready):
                startup_budgets.append(timeout)
                # Three successful bounded starts consume 36 minutes total.
                now[0] += 720
            return result

        with mock.patch.object(acceptance, "time", clock), \
             mock.patch.object(self.executor, "run", side_effect=child):
            result = self.run_admitted()
        self.assertEqual(now[0], 2160)
        self.assertEqual(startup_budgets, [2580, 1860, 1140])
        self.assertTrue(all(check["status"] == "PASS" for check in result["checks"]))
        self.assertTrue(any(check["stage"] == "privacy" and check["assertion"] == "evidence_safety"
                            and check["status"] == "PASS" for check in result["checks"]))
        self.assertTrue(any(check["assertion"] == "plain_down_preserves_volumes" and check["status"] == "PASS"
                            for check in result["checks"]))
        self.assertEqual(result["public_checks"], "UNVERIFIED")

    def test_long_initial_startup_uses_remaining_run_budget_without_false_timeout(self):
        now = [0.0]
        budgets = []
        original = self.executor.run
        clock = mock.Mock()
        clock.monotonic.side_effect = lambda: now[0]

        def child(argv, timeout, input_text=None):
            self.assertLess(now[0], 2580)
            self.assertLessEqual(timeout, 2580 - now[0])
            result = original(argv, timeout, input_text)
            if ("concord-selfhost" in " ".join(argv) and "up" in argv and
                    self.executor.log_boundary and self.executor.tls_ready):
                budgets.append(timeout)
                required = 1860 if len(budgets) == 1 else 60
                # Model the executor's timeout boundary, rather than accepting
                # a successful result whose elapsed time exceeds its budget.
                now[0] += min(required, timeout)
                if required > timeout:
                    return acceptance.CommandResult(124, "", "owned startup timed out")
            return result

        with mock.patch.object(acceptance, "time", clock), \
             mock.patch.object(self.executor, "run", side_effect=child):
            result = self.run_admitted()
        self.assertEqual(now[0], 1980)
        self.assertEqual(budgets, [2580, 720, 660])
        self.assertTrue(all(check["status"] == "PASS" for check in result["checks"]))
        self.assertTrue(any(check["stage"] == "privacy" and check["status"] == "PASS" for check in result["checks"]))

    def test_valid_full_caller_finishes_after_38_minutes_before_43_minute_deadline(self):
        now = [0.0]
        budgets = []
        original = self.executor.run
        clock = mock.Mock()
        clock.monotonic.side_effect = lambda: now[0]

        def child(argv, timeout, input_text=None):
            self.assertLess(now[0], 2580, "no command after the shared deadline")
            self.assertLessEqual(timeout, 2580 - now[0])
            result = original(argv, timeout, input_text)
            if ("concord-selfhost" in " ".join(argv) and "up" in argv and
                    self.executor.log_boundary and self.executor.tls_ready):
                budgets.append(timeout)
                now[0] += min(800, timeout)
                if timeout < 800:
                    return acceptance.CommandResult(124, "", "owned startup exhausted its budget")
            return result

        with mock.patch.object(acceptance, "time", clock), \
             mock.patch.object(self.executor, "run", side_effect=child):
            result = self.run_admitted()
        self.assertEqual(now[0], 2400)
        self.assertEqual(budgets, [2580, 1780, 980])
        self.assertTrue(all(check["status"] == "PASS" for check in result["checks"]))
        self.assertTrue(any(check["assertion"] == "plain_down_preserves_volumes" and check["status"] == "PASS"
                            for check in result["checks"]))
        self.assertTrue(any(check["stage"] == "privacy" and check["status"] == "PASS" for check in result["checks"]))
        self.assertEqual(result["public_checks"], "UNVERIFIED")

    def test_executor_timeout_retains_finite_phase_after_private_capture_prefix(self):
        program = ("import sys,time; sys.stdout.write('PRIVATE_CAPTURE_PREFIX'*4096+'\\n'); "
                   "sys.stdout.write('voice-enforcement-rollout: starting the control-plane in compatibility mode\\n'); "
                   "sys.stdout.flush(); time.sleep(30)")
        result = acceptance.SubprocessExecutor(self.workspace).run([sys.executable, "-c", program], timeout=0.5)
        self.assertEqual(result.returncode, 124)
        self.assertLessEqual(len(result.stdout.encode()), 65536)
        self.assertEqual(result.startup_phase, "startup_cp_ready")
        self.assertNotIn("voice-enforcement-rollout", result.stdout)

    def test_startup_timeout_adds_only_closed_phase_evidence(self):
        cases = [("", "startup_cp_ready", "startup_cp_ready"),
                 ("voice-enforcement-rollout: building the new media-plane image", None, "startup_mp_build"),
                 ("PRIVATE_UNKNOWN_OUTPUT", "PRIVATE_SUSPICIOUS_HINT", None),
                 ("PRIVATE_UNKNOWN_OUTPUT", "startup_mp_diag_command", None),
                 ("PRIVATE_UNKNOWN_OUTPUT", "evidence_safety", None),
                 ("prefix voice-enforcement-rollout: building the new media-plane image", None, None),
                 ("rebuild: starting the remaining services: coturn PRIVATE_SERVICE", None, None)]
        cases = [(*case, False) for case in cases] + [("", "startup_cp_ready", "startup_cp_ready", True)]
        for stdout, hint, phase, expires in cases:
            with self.subTest(hint=hint, phase=phase, expires=expires):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.fail_startup = True
                self.executor.startup_returncode = 124
                self.executor.startup_stdout = stdout
                self.executor.startup_phase = hint
                self.reset_install_state()
                now = [0.0]
                clock = mock.Mock()
                clock.monotonic.side_effect = lambda: now[0]
                original = self.executor.run

                def child(argv, timeout, input_text=None):
                    self.assertLess(now[0], 2580, "no child after timeout consumes shared deadline")
                    result = original(argv, timeout, input_text)
                    if expires and result.returncode == 124:
                        now[0] = 2580
                    return result

                with mock.patch.object(acceptance, "time", clock), \
                     mock.patch.object(self.executor, "run", side_effect=child):
                    with self.assertRaises(acceptance.AcceptanceError) as caught:
                        self.run_admitted()
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                self.assertEqual(caught.exception.returncode, 124)
                record = self.assert_failure_evidence("COMMAND_FAILED")
                expected = [{"stage": "observe", "assertion": "startup_timeout", "status": "FAIL", "code": "COMMAND_FAILED"}]
                if phase:
                    expected.insert(0, {"stage": "observe", "assertion": phase, "status": "FAIL", "code": "COMMAND_FAILED"})
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"], expected)
                for private in ("PRIVATE_UNKNOWN_OUTPUT", "PRIVATE_SUSPICIOUS_HINT", "PRIVATE_SERVICE", self.executor.failure_sentinel):
                    self.assertNotIn(private, json.dumps(record))
                self.assertFalse(any(check["stage"] == "observe" and check["status"] == "PASS" for check in record["checks"]))

    def test_entire_tracked_source_inventory_is_preserved_without_following_symlinks(self):
        external = self.root / "unreadable-external-owned"
        external.write_text("PRIVATE_OUTSIDE_TARGET")
        external.chmod(0)
        link = self.workspace / "owned-tracked-link"
        link.symlink_to(external)
        self.executor.tracked_paths.append("owned-tracked-link")
        original_open = Path.open

        def owned_open(path, *args, **kwargs):
            if path in {link, external}:
                raise AssertionError("tracked symlink inventory must not read its target")
            return original_open(path, *args, **kwargs)

        try:
            with mock.patch.object(Path, "open", new=owned_open):
                self.assertEqual(self.run_admitted()["public_checks"], "UNVERIFIED")
        finally:
            external.chmod(0o600)
        for variant in ("outside-old-list", "mode", "new-tracked-path", "symlink-target", "index-dirty"):
            with self.subTest(variant=variant):
                self.reset_install_state()
                # Restore the owned input so each case starts with a clean fixture.
                dockerfile = self.workspace / "services/media-plane/Dockerfile"
                dockerfile.write_bytes((ROOT / "services/media-plane/Dockerfile").read_bytes())
                dockerfile.chmod(0o644)
                if link.is_symlink():
                    link.unlink()
                link.symlink_to(external)
                self.executor = _OwnedExecutor(self.workspace)

                def mutate(executor):
                    if variant == "outside-old-list":
                        dockerfile.write_text("PRIVATE_TRACKED_MUTATION")
                    elif variant == "mode":
                        dockerfile.chmod(0o755)
                    elif variant == "new-tracked-path":
                        path = self.workspace / "new-owned-tracked-input"
                        path.write_text("PRIVATE_NEW_TRACKED_INPUT")
                        executor.tracked_paths.append(path.name)
                    elif variant == "symlink-target":
                        link.unlink()
                        link.symlink_to(self.root / "another-owned-target")
                    else:
                        executor.git_dirty = True

                self.executor.source_mutation = mutate
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "EVIDENCE_INVALID")
                record = self.assert_failure_evidence("EVIDENCE_INVALID")
                self.assertNotIn("PRIVATE_TRACKED_MUTATION", json.dumps(record))
                self.assertNotIn("PRIVATE_OUTSIDE_TARGET", json.dumps(record))

    def test_dirty_initial_index_refuses_before_installer(self):
        self.executor.git_dirty = True
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "EVIDENCE_INVALID")
        self.assertFalse(any(event[0].startswith("installer-") for event in self.executor.events))

    def test_final_source_chunk_cannot_finish_after_shared_deadline(self):
        runner = self.runner()
        runner.deadline = 10
        now = [0.0]
        clock = mock.Mock()
        clock.monotonic.side_effect = lambda: now[0]
        original_hash = acceptance.hashlib.sha256

        class SlowOwnedHash:
            def __init__(self):
                self.digest = original_hash()

            def update(self, content):
                self.digest.update(content)
                now[0] = 10

            def hexdigest(self):
                return self.digest.hexdigest()

        with mock.patch.object(acceptance, "time", clock), \
             mock.patch.object(acceptance.hashlib, "sha256", side_effect=SlowOwnedHash):
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                runner._source_identity("services/media-plane/Dockerfile")
        self.assertEqual(caught.exception.code, "COMMAND_FAILED")
        self.assertEqual(caught.exception.returncode, 124)

    def test_late_final_inventory_never_publishes_privacy_success(self):
        now = [0.0]
        clock = mock.Mock()
        clock.monotonic.side_effect = lambda: now[0]
        original = acceptance.AcceptanceRunner._source_identity
        final_reads = []

        def identity(runner, name):
            result = original(runner, name)
            if runner.source_bytes:
                final_reads.append(name)
                if len(final_reads) == len(runner.source_bytes):
                    now[0] = 2580
            return result

        with mock.patch.object(acceptance, "time", clock), \
             mock.patch.object(acceptance.AcceptanceRunner, "_source_identity", new=identity):
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                self.run_admitted()
        self.assertTrue(final_reads)
        self.assertEqual(caught.exception.code, "COMMAND_FAILED")
        self.assertEqual(caught.exception.returncode, 124)
        record = self.assert_failure_evidence("COMMAND_FAILED")
        self.assertFalse(any(check["stage"] == "privacy" and check["status"] == "PASS"
                             for check in record["checks"]))

    def test_shipped_log_wrapper_preserves_options_and_specific_refusal(self):
        model = self.root / "owned-log-model.json"
        model.write_text(json.dumps({"services": {"postgres": {}, "pgadmin": {"profiles": ["tools"]}}}))
        calls = self.root / "owned-log-calls"
        config_calls = self.root / "owned-log-calls.config"
        program = '''source "$1"
MODEL="$2"
CALLS="$3"
CONFIG_RC="$4"
LOG_RC="$5"
shift 5
compose() {
  local -a original=("$@")
  if [[ "${1:-}" == --profile ]]; then
    [[ "${2:-}" == '*' ]] || return 99
    shift 2
  fi
  if [[ "$1" == config ]]; then
    printf '%s\\0' "${original[@]}" > "${CALLS}.config"
    if [[ "$CONFIG_RC" != 0 ]]; then return "$CONFIG_RC"; fi
    cat "$MODEL"
  elif [[ "$1" == logs ]]; then
    printf '%s\\0' "${original[@]}" > "$CALLS"
    return "$LOG_RC"
  else
    return 99
  fi
}
verb_logs "$@"
'''
        cases = [(["--no-follow", "--tail=20", "postgres"], 0, 0,
                  ["--profile", "*", "logs", "--tail=20", "postgres"], False),
                 (["--index", "1", "postgres"], 0, 0,
                  ["--profile", "*", "logs", "-f", "--index", "1", "postgres"], False),
                 (["postgres"], 0, 0, ["--profile", "*", "logs", "-f", "postgres"], False),
                 ([], 0, 0, ["logs", "-f"], False),
                 (["unknown-owned-service"], 0, 0, None, True),
                 # Declared inactive-profile services remain valid read-only
                 # recovery targets; logs does not activate their profile.
                 (["pgadmin"], 0, 0, ["--profile", "*", "logs", "-f", "pgadmin"], False),
                 (["postgres"], 17, 0, None, False),
                 (["postgres"], 0, 18, ["--profile", "*", "logs", "-f", "postgres"], False)]
        conflicting_flags = ("-f", "--follow", "--follow=true", "--follow=false", "-tf", "-ft")
        cases += [(["--no-follow", flag, "postgres"], 0, 1, None, False) for flag in conflicting_flags]
        for option, value in (("--since", "-f"), ("--until", "--follow")):
            cases.append((["--no-follow", option, value, "postgres"], 0, 0,
                          ["--profile", "*", "logs", option, value, "postgres"], False))
        cases.append((["--follow", "postgres"], 0, 0,
                      ["--profile", "*", "logs", "-f", "--follow", "postgres"], False))
        for arguments, config_rc, logs_rc, expected_call, specific in cases:
            with self.subTest(arguments=arguments, config_rc=config_rc, logs_rc=logs_rc):
                calls.unlink(missing_ok=True)
                config_calls.unlink(missing_ok=True)
                child = subprocess.run(["bash", "-c", program, "owned-wrapper-test",
                                        str(ROOT / "infrastructure/deploy/concord-selfhost"), str(model), str(calls),
                                        str(config_rc), str(logs_rc), *arguments],
                                       env={"PATH": os.environ["PATH"], "HOME": str(self.root)},
                                       text=True, capture_output=True, timeout=5, check=False)
                self.assertEqual(child.returncode, 1 if specific else config_rc or logs_rc)
                self.assertEqual("selfhost-storage: unknown log service" in child.stderr, specific)
                conflict = bool(arguments and arguments[0] == "--no-follow" and arguments[1] in conflicting_flags)
                if conflict:
                    self.assertFalse(config_calls.exists(), "conflict must refuse before even querying Compose")
                    self.assertIn("selfhost-storage: conflicting log follow options", child.stderr)
                else:
                    self.assertEqual(config_calls.read_bytes().decode().split("\0")[:-1],
                                     ["--profile", "*", "config", "--format", "json"])
                if expected_call is None:
                    self.assertFalse(calls.exists())
                else:
                    self.assertEqual(calls.read_bytes().decode().split("\0")[:-1], expected_call)

    def test_project_state_requires_complete_unambiguous_missing_container_response(self):
        names = [CONTAINER_NAMES[service] for service in sorted(SERVICES)]
        modern = [f"Error response from daemon: No such container: {name}" for name in names]
        legacy = [f"Error: No such object: {name}" for name in names]

        def state(result):
            runner = self.runner()
            runner.project = PROJECT
            empty = acceptance.CommandResult(0, "", "")
            with mock.patch.object(self.executor, "run", side_effect=[empty, empty, result]) as child:
                value = runner._project_state()
            self.assertEqual(child.call_args_list[-1].args[0][-len(names):], names)
            return value

        for lines in (modern, legacy):
            with self.subTest(valid_response=lines[0]):
                self.assertEqual(state(acceptance.CommandResult(1, "", "\n".join(lines))), ("", ""))

        invalid = {
            "partial stdout": acceptance.CommandResult(1, '{"Id":"existing-container"}\n', "\n".join(modern)),
            "only one missing name": acceptance.CommandResult(1, "", modern[0]),
            "duplicate instead of complete names": acceptance.CommandResult(1, "", "\n".join(modern[:-1] + [modern[0]])),
            "unrequested missing name": acceptance.CommandResult(1, "", "\n".join(modern[:-1] + ["Error response from daemon: No such container: unrelated-owned-name"])),
            "unrelated daemon error": acceptance.CommandResult(1, "", "\n".join(modern + ["Error response from daemon: permission denied"])),
            "missing phrase in unrelated error": acceptance.CommandResult(1, "", "transport failed: No such object"),
            "wrong return code": acceptance.CommandResult(2, "", "\n".join(modern)),
            "empty error": acceptance.CommandResult(1, "", ""),
        }
        for label, result in invalid.items():
            with self.subTest(invalid_response=label):
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    state(result)
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")

    def run_admitted(self, *, env: dict | None = None, uid: int = 1000):
        with mock.patch.dict(os.environ, env or self.HOSTED, clear=True), \
             mock.patch.object(acceptance.os, "getuid", return_value=uid):
            return self.runner().run()

    def test_docker_security_floor_and_metadata_produce_canonical_evidence(self):
        for stdout, canonical in (("29.8.2", "29.8.2"), ("29.8.2\n", "29.8.2"),
                                  ("29.8.2+dfsg1\n", "29.8.2"),
                                  ("29.8.3\n", "29.8.3"), ("30.0.0\n", "30.0.0")):
            with self.subTest(stdout=stdout):
                self.reset_install_state()
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.docker_version_result = acceptance.CommandResult(0, stdout, "")
                result = self.run_admitted()
                self.assertEqual(result["tool_versions"], {"docker": canonical, "compose": "2.24.5"})
                recorded = json.loads((self.evidence / "evidence.json").read_text())
                self.assertEqual(recorded["tool_versions"]["docker"], canonical)

    def test_docker_below_floor_malformed_and_failed_queries_refuse_before_mutation(self):
        invalid = ["29.8.1\n", "29.8.1+dfsg1\n", "28.3.3\n", "", "garbage\n",
                   "v29.8.2\n", "029.8.2\n", "29.08.2\n", "29.8.02\n", "29.8\n",
                   "29.8.2-rc1\n", "29.8.2~rc1\n", "29.8.2+\n", "29.8.2+bad_metadata\n",
                   " 29.8.2\n", "29.8.2 \n", "29.8.2\r\n", "29.8.2\n\n",
                   "29.8.2\n29.8.3\n", "29.8.2 REGISTRY_SENTINEL\n"]
        cases = [(0, stdout) for stdout in invalid] + [(42, "29.8.2\n")]
        for status, stdout in cases:
            with self.subTest(status=status, stdout=stdout):
                self.reset_install_state()
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.docker_version_result = acceptance.CommandResult(status, stdout, "REGISTRY_SENTINEL")
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "TOOL_VERSION_INVALID")
                record = self.assert_failure_evidence("TOOL_VERSION_INVALID")
                self.assertNotIn("REGISTRY_SENTINEL", json.dumps(record))
                self.assert_no_mutations()

    def test_nginx_activation_failure_uses_finite_boundary_context(self):
        for boundary in ("nginx_vhost", "nginx_config"):
            with self.subTest(boundary=boundary):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.nginx_failure = boundary
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                record = self.assert_failure_evidence("COMMAND_FAILED")
                failures = [check for check in record["checks"] if check["status"] == "FAIL"]
                self.assertEqual(failures, [{"stage": "render", "assertion": boundary,
                                            "status": "FAIL", "code": "COMMAND_FAILED"}])
                self.assertNotIn("owned", json.dumps(record))
                self.assertFalse(any(check["status"] == "PASS" and
                                     check["assertion"].startswith("nginx_")
                                     for check in record["checks"]))
                self.assertFalse(any(event[0] == "wrapper" and event[1] == "up"
                                     for event in self.executor.events))

    def test_nginx_activation_validates_fixed_isolated_configuration_before_start(self):
        runner = self.runner()
        rendered = self.workspace / "infrastructure/deploy/nginx/concordvoice.rendered.conf"
        rendered.write_text("owned inert rendered-vhost fixture\n", encoding="utf-8")
        with mock.patch.object(runner, "_run", return_value=acceptance.CommandResult(0, "", "")) as command:
            runner._activate_nginx()
        self.assertEqual([call.args[0] for call in command.call_args_list], [
            ["sudo", "-n", "install", "-o", "root", "-g", "root", "-m", "0644",
             str(rendered), "/etc/nginx/conf.d/concordvoice.conf"],
            ["sudo", "-n", "systemctl", "enable", "nginx"],
            ["sudo", "-n", "nginx", "-t", "-c", "/etc/nginx/concord-selfhost-ci.conf"],
            ["sudo", "-n", "systemctl", "start", "nginx"],
            ["sudo", "-n", "systemctl", "is-active", "--quiet", "nginx"],
            ["sudo", "-n", "systemctl", "reload", "nginx"],
        ], "nginx activation must validate fixed CI configuration before unchanged service lifecycle")

    def test_owned_nginx_command_model_refuses_default_or_wrong_configuration(self):
        for arguments in (["nginx", "-t"], ["nginx", "-t", "-c", "/etc/nginx/nginx.conf"],
                          ["nginx", "-t", "-c", "/etc/nginx/concord-selfhost-ci.conf.bak"]):
            with self.subTest(arguments=arguments):
                with self.assertRaisesRegex(AssertionError, "nginx syntax must use isolated"):
                    self.executor.run(["sudo", "-n", *arguments], timeout=5)
        self.assertEqual(self.executor.run(["sudo", "-n", "nginx", "-t", "-c",
                                           "/etc/nginx/concord-selfhost-ci.conf"], timeout=5).returncode, 0)

    def test_nginx_service_failures_name_exact_operation_and_stop_before_observation(self):
        operations = (
            ("nginx_service_enable", 41, ["sudo", "-n", "systemctl", "enable", "nginx"]),
            ("nginx_service_start", 42, ["sudo", "-n", "systemctl", "start", "nginx"]),
            ("nginx_service_active", 43, ["sudo", "-n", "systemctl", "is-active", "--quiet", "nginx"]),
            ("nginx_service_reload", 44, ["sudo", "-n", "systemctl", "reload", "nginx"]),
        )
        for index, (assertion, child_exit, command) in enumerate(operations):
            with self.subTest(operation=assertion):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.nginx_failure = assertion
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                self.assertEqual(caught.exception.returncode, child_exit)
                attempted = []
                known_children = [row[2][2:] for row in operations]
                for call in self.executor.calls:
                    if call[7:] in known_children:
                        self.assertEqual(call[:5], ["sudo", "-n", "/usr/bin/python3", "-I", "-c"])
                        self.assertEqual(call[5], acceptance._PRIVILEGED_SUPERVISOR)
                        self.assertTrue(math.isfinite(float(call[6])))
                        attempted.append(["sudo", "-n", *call[7:]])
                    elif call[:3] == ["sudo", "-n", "systemctl"] and call[-1] == "nginx":
                        self.fail("nginx service operation bypassed privileged supervisor")
                self.assertEqual(attempted, [row[2] for row in operations[:index + 1]])
                self.assertEqual(attempted[-1], command)
                record = self.assert_failure_evidence("COMMAND_FAILED")
                self.assertEqual([row for row in record["checks"] if row["status"] == "FAIL"],
                                 [{"stage": "render", "assertion": assertion,
                                   "status": "FAIL", "code": "COMMAND_FAILED"}])
                self.assertFalse(any(row["stage"] in {"observe", "lifecycle"} for row in record["checks"]))
                self.assertFalse(any(event[0] == "wrapper" and event[1] == "up"
                                     for event in self.executor.events))
                self.assertEqual(record["public_checks"], "UNVERIFIED")
                serialized = json.dumps(record)
                for private in ("PRIVATE_NGINX_STDOUT", "PRIVATE_NGINX_ENV", "opaque",
                                "sudo", "systemctl", str(self.workspace)):
                    self.assertNotIn(private, serialized)

    def test_failed_startup_records_only_last_exact_owned_progress_context(self):
        markers = [
            ("rebuild: starting the voice services' dependencies: postgres redis nats", "startup_dependencies"),
            ("voice-enforcement-rollout: building the new control-plane image", "startup_cp_build"),
            ("voice-enforcement-rollout: starting the control-plane in compatibility mode", "startup_cp_ready"),
            ("voice-enforcement-rollout: building the new media-plane image", "startup_mp_build"),
            ("voice-enforcement-rollout: starting the new media-plane", "startup_mp_ready"),
            ("voice-enforcement-rollout: waiting for media-plane liveness", "startup_mp_liveness"),
            ("voice-enforcement-rollout: waiting for media-plane protocol 3 readiness", "startup_mp_protocol"),
            ("voice-enforcement-rollout: activating durable enforcement", "startup_enforcement"),
            ("rebuild: starting the remaining services: coturn minio ops-agent", "startup_remainder"),
            ("voice-enforcement-rollout: completed", "startup_postcheck"),
        ]
        cases = [("\n".join(line for line, _ in markers[:index + 1]), context, 37)
                 for index, (_line, context) in enumerate(markers)]
        # Observations follow output order, rather than an invented phase rank.
        cases += [(markers[-1][0] + "\n" + markers[0][0], "startup_dependencies", 37),
                  (markers[1][0], "startup_timeout", 124),
                  ("", "selected_stack", 37)]
        decoys = ["prefix " + markers[1][0], markers[1][0] + " altered",
                  "rebuild: starting the voice services' dependencies: postgres unknown-service",
                  "rebuild: starting the remaining services: coturn secret-value",
                  "voice-enforcement-rollout: unrecognized phase"]
        cases += [(line, "selected_stack", 37) for line in decoys]
        cases.append((markers[2][0] + "\n" + "\n".join(decoys), "startup_cp_ready", 37))
        media_liveness = markers[5][0]
        media_protocol = markers[6][0]
        # Exact observations distinguish Compose liveness from the controller's
        # subsequent protocol gate, while legacy output keeps its old context.
        media_decoys = ["prefix " + media_liveness, media_liveness + " altered",
                        "prefix " + media_protocol, media_protocol + " altered"]
        cases += [(line, "selected_stack", 37) for line in media_decoys]
        cases += [(markers[4][0] + "\n" + "\n".join(media_decoys), "startup_mp_ready", 37),
                  (media_protocol + "\n" + media_liveness, "startup_mp_liveness", 37),
                  (media_liveness + "\n" + media_protocol, "startup_mp_protocol", 37),
                  (media_protocol + "\n" + "\n".join(media_decoys), "startup_mp_protocol", 37)]
        # The default fake has unavailable diagnostics (a nonzero inspect), so
        # media liveness now reports that finite diagnostic boundary explicitly.
        cases = [(stdout, "startup_mp_diag_command" if context == "startup_mp_liveness" else context, rc)
                 for stdout, context, rc in cases]
        for stdout, context, returncode in cases:
            with self.subTest(context=context, stdout=stdout, returncode=returncode):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.fail_startup = True
                self.executor.startup_stdout = stdout
                self.executor.startup_returncode = returncode
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                record = self.assert_failure_evidence("COMMAND_FAILED")
                expected_failures = [{"stage": "observe", "assertion": context,
                                      "status": "FAIL", "code": "COMMAND_FAILED"}]
                if returncode == 124:
                    expected_failures.insert(0, {"stage": "observe", "assertion": "startup_cp_build",
                                                 "status": "FAIL", "code": "COMMAND_FAILED"})
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"], expected_failures)
                serialized = json.dumps(record)
                self.assertNotIn(self.executor.failure_sentinel, serialized)
                self.assertNotIn("secret-value", serialized)
                self.assertFalse(any(check["status"] == "PASS" and
                                     (check["stage"] == "observe" or
                                      check["assertion"].startswith("startup_"))
                                     for check in record["checks"]))
                self.assertFalse(any(event[0] == "route" for event in self.executor.events))
                self.assertEqual(self.executor.guard_count, {"log": 1, "tls": 1})

    def test_media_liveness_failure_diagnostics_are_owned_bounded_and_private(self):
        def projection(**changes):
            state = {"Status": "running", "Running": True, "Restarting": False,
                     "OOMKilled": False, "ExitCode": 0, "Health": {"Status": "healthy"}}
            state.update(changes)
            return {"project": PROJECT, "service": "media-plane", "state": state}

        def inspect(value, code=0):
            return acceptance.CommandResult(code, json.dumps(value), "")

        def fatal(code, **changes):
            record = {"message": "Fatal error during startup", "error": {"code": code},
                      "private": "PRIVATE_DIAGNOSTIC_SENTINEL"}
            record.update(changes)
            return json.dumps(record)

        cases = [
            ("oom", inspect(projection(OOMKilled=True)), fatal("EACCES"), "startup_mp_oom"),
            ("unhealthy", inspect(projection(Health={"Status": "unhealthy"})), "", "startup_mp_unhealthy"),
            ("restarting", inspect(projection(Status="restarting", Running=True, Restarting=True)), "", "startup_mp_restarting"),
            ("exited", inspect(projection(Status="exited", Running=False, ExitCode=1)), "", "startup_mp_exited"),
            ("created", inspect(projection(Status="created", Running=False)), "", "startup_mp_created"),
            ("running", inspect(projection()), "", "startup_mp_running"),
        ]
        mapping = {"EACCES": "permission", "EROFS": "readonly", "ENOENT": "missing_file",
                   "MODULE_NOT_FOUND": "missing_module", "ERR_MODULE_NOT_FOUND": "missing_module",
                   "ERR_DLOPEN_FAILED": "native_load", "ENOMEM": "memory",
                   "EAGAIN": "resource", "EMFILE": "resource", "ENFILE": "resource"}
        cases += [(code, inspect(projection()), fatal(code), "startup_mp_log_" + context)
                  for code, context in mapping.items()]
        cases += [
            ("last fatal", inspect(projection()), fatal("EACCES") + "\n" + fatal("ENOENT"), "startup_mp_log_missing_file"),
            ("nonfatal", inspect(projection()), fatal("EACCES", message="Routine startup event"), "startup_mp_running"),
            ("embedded", inspect(projection()), "prefix " + fatal("EACCES"), "startup_mp_running"),
            ("unknown", inspect(projection()), fatal("UNKNOWN_PRIVATE_CODE"), "startup_mp_running"),
            ("invalid code type", inspect(projection()), fatal(["EACCES"]), "startup_mp_running"),
            ("invalid json", inspect(projection()), "{PRIVATE_DIAGNOSTIC_SENTINEL", "startup_mp_running"),
            ("wrong project", inspect(dict(projection(), project="foreign-project")), fatal("EACCES"), "startup_mp_diag_identity"),
            ("wrong service", inspect(dict(projection(), service="control-plane")), fatal("EACCES"), "startup_mp_diag_identity"),
            ("invalid bool", inspect(projection(Running="true")), fatal("EACCES"), "startup_mp_diag_invalid"),
            ("invalid exit code", inspect(projection(ExitCode=True)), fatal("EACCES"), "startup_mp_diag_invalid"),
            ("invalid health", inspect(projection(Health={"Status": 3})), fatal("EACCES"), "startup_mp_diag_invalid"),
            ("invalid oom type", inspect(projection(OOMKilled=1)), fatal("EACCES"), "startup_mp_diag_invalid"),
            ("invalid restarting type", inspect(projection(Restarting="false")), fatal("EACCES"), "startup_mp_diag_invalid"),
            ("invalid status type", inspect(projection(Status=False)), fatal("EACCES"), "startup_mp_diag_invalid"),
            ("invalid shape", inspect([]), fatal("EACCES"), "startup_mp_diag_invalid"),
            ("malformed projection", acceptance.CommandResult(0, "{PRIVATE_DIAGNOSTIC_SENTINEL", ""), "", "startup_mp_diag_invalid"),
            ("inspect failed", acceptance.CommandResult(1, "", "PRIVATE_DIAGNOSTIC_SENTINEL"), fatal("EACCES"), "startup_mp_diag_command"),
            ("inspect timeout", acceptance.CommandResult(124, "", "PRIVATE_DIAGNOSTIC_SENTINEL"), "", "startup_mp_diag_timeout"),
            ("inspect exception", RuntimeError("PRIVATE_DIAGNOSTIC_SENTINEL"), "", "startup_mp_diag_unavailable"),
            ("oversized inspect", acceptance.CommandResult(0, "X" * 65537, ""), "", "startup_mp_diag_limit"),
            ("oversized combined output", inspect(projection()), "X" * 65537, "startup_mp_running"),
            ("logs exception", inspect(projection()), RuntimeError("PRIVATE_DIAGNOSTIC_SENTINEL"), "startup_mp_running"),
            ("logs command failure", inspect(projection()), acceptance.CommandResult(19, "", "PRIVATE_DIAGNOSTIC_SENTINEL"), "startup_mp_running"),
            ("missing container", acceptance.CommandResult(1, "", "Error response from daemon: No such container: concordvoice-media-plane\n"), "", "startup_mp_diag_missing"),
            ("legacy missing container", acceptance.CommandResult(1, "", "Error: No such object: concordvoice-media-plane\n"), "", "startup_mp_diag_missing"),
            ("wrong missing target", acceptance.CommandResult(1, "", "Error response from daemon: No such container: unrelated-owned-name"), "", "startup_mp_diag_command"),
            ("mixed missing error", acceptance.CommandResult(1, "", "Error response from daemon: No such container: concordvoice-media-plane\npermission denied"), "", "startup_mp_diag_command"),
            ("missing with partial stdout", acceptance.CommandResult(1, "partial PRIVATE_DIAGNOSTIC_SENTINEL", "Error response from daemon: No such container: concordvoice-media-plane"), "", "startup_mp_diag_command"),
            ("embedded missing", acceptance.CommandResult(1, "", "prefix Error response from daemon: No such container: concordvoice-media-plane"), "", "startup_mp_diag_command"),
            ("daemon unavailable", acceptance.CommandResult(1, "", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"), "", "startup_mp_diag_daemon"),
            ("daemon inaccessible", acceptance.CommandResult(1, "", "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock"), "", "startup_mp_diag_daemon"),
            ("embedded daemon phrase", acceptance.CommandResult(1, "", "unrelated: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"), "", "startup_mp_diag_command"),
            ("mixed daemon error", acceptance.CommandResult(1, "", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\nunrelated error"), "", "startup_mp_diag_command"),
        ]
        for label, inspected, logs, expected in cases:
            with self.subTest(scenario=label):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.fail_startup = True
                self.executor.startup_stdout = "voice-enforcement-rollout: waiting for media-plane liveness"
                self.executor.startup_inspect_result = inspected
                self.executor.startup_logs_result = (acceptance.CommandResult(0, logs, "")
                                                     if isinstance(logs, str) else logs)
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                self.assertEqual(caught.exception.returncode, 37)
                record = self.assert_failure_evidence("COMMAND_FAILED")
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                                 [{"stage": "observe", "assertion": expected,
                                   "status": "FAIL", "code": "COMMAND_FAILED"}])
                serialized = json.dumps(record)
                for private in (self.executor.failure_sentinel, "PRIVATE_DIAGNOSTIC_SENTINEL",
                                "UNKNOWN_PRIVATE_CODE", "foreign-project"):
                    self.assertNotIn(private, serialized)
                self.assertFalse(any(check["status"] == "PASS" and check["stage"] == "observe"
                                     for check in record["checks"]))
                self.assertFalse(any(event[0] == "route" for event in self.executor.events))
                self.assertEqual(self.executor.guard_count, {"log": 1, "tls": 1})
                self.assertTrue(any(event == ("startup-diagnostic", "inspect")
                                    for event in self.executor.events))
                self.assertFalse(any(event[0] == "wrapper" and event[1] in {"stop", "down"}
                                     for event in self.executor.events))

    def test_selected_model_cpu_capacity_refuses_before_any_startup(self):
        # The shipped basic selection must fit a two-CPU private hosted runner;
        # managed production's three-worker quota is not this selected model.
        model = rendered_model()
        self.assertEqual(model["services"]["media-plane"]["deploy"]["resources"]["limits"]["cpus"], "2")
        self.assertEqual(model["services"]["media-plane"]["environment"]["NUM_WORKERS"], "2")
        self.executor.daemon_cpus = 2
        self.assertEqual(self.run_admitted()["public_checks"], "UNVERIFIED")

        def set_quota(service, value):
            return lambda model: model["services"][service]["deploy"]["resources"]["limits"].__setitem__("cpus", value)

        def set_workers(value):
            return lambda model: model["services"]["media-plane"]["environment"].__setitem__("NUM_WORKERS", value)

        def original_production(model):
            set_quota("media-plane", "3")(model)
            set_workers("3")(model)

        mutations = [("original production 3/3", original_production),
                     ("non-media quota over capacity", set_quota("postgres", "2.5")),
                     ("missing quota", lambda model: model["services"]["media-plane"]["deploy"]["resources"]["limits"].pop("cpus")),
                     ("missing workers", lambda model: model["services"]["media-plane"]["environment"].pop("NUM_WORKERS"))]
        mutations += [(f"invalid quota {value!r}", set_quota("media-plane", value))
                      for value in (None, True, 0, -1, "", "not-a-number", "NaN", "Infinity", [], {})]
        mutations += [(f"invalid workers {value!r}", set_workers(value))
                      for value in (None, True, 0, -1, "0", "3", "1.5", "", "not-an-integer", [], {})]
        for label, mutation in mutations:
            with self.subTest(scenario=label):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.capacity_model_mutation = mutation
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "HOST_CAPACITY_INVALID")
                record = self.assert_failure_evidence("HOST_CAPACITY_INVALID")
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                                 [{"stage": "render", "assertion": "host_capacity",
                                   "status": "FAIL", "code": "HOST_CAPACITY_INVALID"}])
                self.assertFalse(any("concord-selfhost" in " ".join(call) and "up" in call
                                     for call in self.executor.calls))
                self.assertFalse(any("docker" in call and "build" in call for call in self.executor.calls))
                self.assertFalse(any(event[0] == "route" for event in self.executor.events))

    def test_daemon_cpu_capacity_requires_strict_integer_host_floor(self):
        for value in (None, True, False, 0, 1, -1, "2", 2.0, [], {}):
            with self.subTest(ncpu=value):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.daemon_cpus = value
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "HOST_FLOOR_INVALID")
                self.assert_failure_evidence("HOST_FLOOR_INVALID")
                self.assert_no_mutations()

    def assert_failure_evidence(self, code: str):
        path = self.evidence / "evidence.json"
        self.assertTrue(path.is_file())
        record = json.loads(path.read_text(encoding="utf-8"))
        self.assertTrue(any(check["status"] == "FAIL" and check["code"] == code
                            for check in record["checks"]))
        return record

    def assert_no_mutations(self):
        verbs = {"up", "stop", "down", "restart", "rebuild", "freshstart", "create",
                 "start", "rm", "remove", "prune", "exec"}
        for call in self.executor.calls:
            if "concord-selfhost" in " ".join(call) or "docker" in call and "compose" in call:
                self.assertFalse(any(token in verbs for token in call), call)
            self.assertFalse(any(name in " ".join(call) for name in
                                 ("install-selfhost.sh", "provision-cert.sh",
                                  "provision-production.sh")), call)

    def project_snapshot(self):
        return (self.executor.preexisting_project, self.executor.stack,
                self.executor.volumes_present,
                tuple(sorted((self.workspace / ".env").read_bytes()
                             if (self.workspace / ".env").exists() else b"")))

    def reset_install_state(self):
        (self.workspace / ".env").unlink(missing_ok=True)
        # Each independent refusal gets a fresh evidence directory so a
        # previous artifact cannot satisfy the next scenario's assertion.
        self.evidence_scenario += 1
        self.evidence = self.root / f"evidence-{self.evidence_scenario}"
        self.evidence.mkdir()

    def test_refuses_non_hosted_non_linux_or_root_execution(self):
        non_hosted = dict(self.HOSTED, RUNNER_ENVIRONMENT="self-hosted")
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted(env=non_hosted)
        self.assertEqual(caught.exception.code, "HOST_FLOOR_INVALID")
        self.assert_failure_evidence("HOST_FLOOR_INVALID")
        self.assert_no_mutations()
        self.executor = _OwnedExecutor(self.workspace)
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted(uid=0)
        self.assertEqual(caught.exception.code, "HOST_FLOOR_INVALID")
        self.assert_failure_evidence("HOST_FLOOR_INVALID")
        self.assert_no_mutations()

        self.executor = _OwnedExecutor(self.workspace)
        with mock.patch.object(acceptance.platform, "system", return_value="Darwin"), \
             self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "HOST_FLOOR_INVALID")
        self.assert_failure_evidence("HOST_FLOOR_INVALID")
        self.assert_no_mutations()

    def test_blocked_public_prerequisites_write_sanitized_evidence(self):
        runner_temp = self.root / "runner-temp"
        runner_temp.mkdir()
        evidence = runner_temp / "selfhost-acceptance-evidence"
        env = dict(self.HOSTED, RUNNER_TEMP=str(runner_temp))
        executor = mock.Mock()
        executor.run.return_value = acceptance.CommandResult(0, self.HOSTED["GITHUB_SHA"] + "\n", "")
        with mock.patch.dict(os.environ, env, clear=True), \
             mock.patch.object(Path, "cwd", return_value=self.workspace), \
             mock.patch.object(acceptance.os, "getuid", return_value=1000), \
             mock.patch.object(acceptance, "SubprocessExecutor", return_value=executor):
            result = acceptance.main(["--blocked-prerequisite", "host-packages",
                                      "--evidence-dir", str(evidence)])
        self.assertEqual(result, 0)
        self.assertEqual(executor.run.call_args.args[0], ["git", "rev-parse", "HEAD"])
        self.assertEqual(evidence.stat().st_mode & 0o777, 0o700)
        artifact = evidence / "evidence.json"
        self.assertTrue(artifact.is_file(),
                        "an apt prerequisite failure must leave evidence for the always-upload step")
        record = json.loads(artifact.read_text(encoding="utf-8"))
        self.assertEqual(record, {
            "schema": 1,
            "candidate_sha": self.HOSTED["GITHUB_SHA"],
            "run_id": self.HOSTED["GITHUB_RUN_ID"],
            "runner_os": "ubuntu-24.04",
            "tool_versions": {"docker": "unavailable", "compose": "unavailable"},
            "local_only": True,
            "checks": [{"stage": "host", "assertion": "host_floors",
                        "status": "BLOCKED", "code": "HOST_PREREQUISITES_FAILED"}],
            "public_checks": "UNVERIFIED",
        })
        self.assertEqual({entry.name for entry in evidence.iterdir()}, {"evidence.json"})

    def test_blacksmith_jit_profile_runs_original_pr_checks_and_schema1(self):
        result = self.run_admitted(env=dict(self.HOSTED, **self.BLACKSMITH_PROFILE))
        self.assertEqual(result["schema"], 1)
        self.assertEqual(result["candidate_sha"], self.HOSTED["GITHUB_SHA"])
        self.assertEqual(result["runner_os"], "ubuntu-24.04")
        self.assertEqual(result["public_checks"], "UNVERIFIED")
        self.assertEqual(Counter((row["stage"], row["assertion"]) for row in result["checks"]),
                         PublicBundleAcceptanceTests.REQUIRED)
        self.assertTrue(all(row["status"] == "PASS" and row["code"] == "OK" for row in result["checks"]))
        self.assertEqual([event[1] for event in self.executor.events if event[0] == "db-read"],
                         ["owned-row", "owned-row", "owned-row"])

    def test_runner_profile_closed_pairs_refuse_before_executor_calls(self):
        changes = (
            {"SELFHOST_ACCEPTANCE_RUNNER_PROFILE": None},
            {"SELFHOST_ACCEPTANCE_RUNNER_PROFILE": ""},
            {"SELFHOST_ACCEPTANCE_RUNNER_PROFILE": "blacksmith-2vcpu-ubuntu-2404"},
            {"SELFHOST_ACCEPTANCE_RUNNER_PROFILE": "unknown"},
            {"GITHUB_REPOSITORY": "untrusted/fixture"}, {"GITHUB_REPOSITORY": None},
            {"RUNNER_ENVIRONMENT": "unknown"}, {"RUNNER_ENVIRONMENT": "github-hosted"},
            {"RUNNER_ENVIRONMENT": "github-hosted", "SELFHOST_ACCEPTANCE_RUNNER_PROFILE": ""},
        )
        for change in changes:
            with self.subTest(change=change):
                self.setUp()
                env = dict(self.HOSTED, **self.BLACKSMITH_PROFILE)
                for key, value in change.items():
                    if value is None:
                        env.pop(key, None)
                    else:
                        env[key] = value
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted(env=env)
                self.assertEqual(caught.exception.code, "HOST_FLOOR_INVALID")
                self.assertEqual(self.executor.calls, [], "profile refusal must precede all owned host calls")
                self.assert_no_mutations()

    def test_blacksmith_pr_blocked_recorder_obeys_the_same_closed_profile_pair(self):
        for case in ("valid", "missing", "unknown", "wrong-repository", "inconsistent-environment", "blank-legacy-profile"):
            with self.subTest(case=case):
                self.setUp()
                env = dict(self.HOSTED, **self.BLACKSMITH_PROFILE, RUNNER_TEMP=str(self.root))
                if case == "missing":
                    env.pop("SELFHOST_ACCEPTANCE_RUNNER_PROFILE")
                elif case == "unknown":
                    env["SELFHOST_ACCEPTANCE_RUNNER_PROFILE"] = "unknown"
                elif case == "wrong-repository":
                    env["GITHUB_REPOSITORY"] = "untrusted/fixture"
                elif case == "inconsistent-environment":
                    env["RUNNER_ENVIRONMENT"] = "github-hosted"
                elif case == "blank-legacy-profile":
                    env["RUNNER_ENVIRONMENT"] = "github-hosted"
                    env["SELFHOST_ACCEPTANCE_RUNNER_PROFILE"] = ""
                with mock.patch.dict(os.environ, env, clear=True), \
                     mock.patch.object(Path, "cwd", return_value=self.workspace), \
                     mock.patch.object(acceptance.os, "getuid", return_value=1000), \
                     mock.patch.object(acceptance, "SubprocessExecutor", return_value=self.executor):
                    result = acceptance.main(["--blocked-prerequisite", "host-packages",
                                              "--evidence-dir", str(self.evidence)])
                self.assertEqual(result, 0 if case == "valid" else 1)
                self.assert_no_mutations()
                if case == "valid":
                    record = json.loads((self.evidence / "evidence.json").read_text())
                    self.assertEqual(record["schema"], 1)
                    self.assertEqual(record["public_checks"], "UNVERIFIED")
                    self.assertEqual(record["checks"], [{"stage": "host", "assertion": "host_floors",
                                                        "status": "BLOCKED", "code": "HOST_PREREQUISITES_FAILED"}])
                else:
                    self.assertEqual(self.executor.calls, [])
                    self.assertFalse((self.evidence / "evidence.json").exists())

    def test_blocked_prerequisite_identity_failures_do_not_claim_apt_failure(self):
        for case, child_result, expected_artifact in (
                ("sha-mismatch", acceptance.CommandResult(0, "b" * 40 + "\n", ""), True),
                ("sha-command-failure", acceptance.CommandResult(128, "", "PRIVATE_APT_SENTINEL"), True),
                ("non-hosted", acceptance.CommandResult(0, self.HOSTED["GITHUB_SHA"] + "\n", ""), False),
                ("outside-runner-temp", acceptance.CommandResult(0, self.HOSTED["GITHUB_SHA"] + "\n", ""), False),
                ("invalid-github-sha", acceptance.CommandResult(0, self.HOSTED["GITHUB_SHA"] + "\n", ""), False)):
            with self.subTest(case=case):
                runner_temp = self.root / f"runner-temp-{case}"
                runner_temp.mkdir()
                evidence = runner_temp / "evidence"
                env = dict(self.HOSTED, RUNNER_TEMP=str(runner_temp))
                if case == "non-hosted":
                    env["RUNNER_ENVIRONMENT"] = "self-hosted"
                if case == "outside-runner-temp":
                    evidence = self.root / "outside-runner-temp-evidence"
                elif case == "invalid-github-sha":
                    env["GITHUB_SHA"] = "not-a-commit"
                executor = mock.Mock()
                executor.run.return_value = child_result
                with mock.patch.dict(os.environ, env, clear=True), \
                     mock.patch.object(Path, "cwd", return_value=self.workspace), \
                     mock.patch.object(acceptance.os, "getuid", return_value=1000), \
                     mock.patch.object(acceptance, "SubprocessExecutor", return_value=executor):
                    result = acceptance.main(["--blocked-prerequisite", "host-packages",
                                              "--evidence-dir", str(evidence)])
                self.assertEqual(result, 1)
                if not expected_artifact:
                    self.assertFalse(evidence.exists())
                    executor.run.assert_not_called()
                    continue
                record = json.loads((evidence / "evidence.json").read_text(encoding="utf-8"))
                self.assertEqual(record["checks"], [{
                    "stage": "host", "assertion": "host_floors", "status": "BLOCKED",
                    "code": "HOST_FLOOR_INVALID",
                }])
                self.assertNotIn("PRIVATE_APT_SENTINEL",
                                 (evidence / "evidence.json").read_text(encoding="utf-8"))

    def test_blocked_prerequisite_cli_refuses_root_without_git_or_evidence(self):
        runner_temp = self.root / "root-cli-runner-temp"
        runner_temp.mkdir()
        evidence = runner_temp / "evidence"
        env = dict(self.HOSTED, RUNNER_TEMP=str(runner_temp))
        executor = mock.Mock()
        with mock.patch.dict(os.environ, env, clear=True), \
             mock.patch.object(Path, "cwd", return_value=self.workspace), \
             mock.patch.object(acceptance.os, "getuid", return_value=0), \
             mock.patch.object(acceptance, "SubprocessExecutor", return_value=executor):
            result = acceptance.main(["--blocked-prerequisite", "host-packages", "--evidence-dir", str(evidence)])
        self.assertEqual(result, 1)
        executor.run.assert_not_called()
        self.assertFalse(evidence.exists())

    def test_cli_blocked_prerequisite_mode_is_exclusive(self):
        with self.assertRaises(SystemExit):
            acceptance.main(["--fresh-host", "--blocked-prerequisite", "host-packages",
                             "--evidence-dir", str(self.root / "evidence")])

    def test_refuses_existing_environment_or_project(self):
        (self.workspace / ".env").write_text("owned synthetic fixture\n", encoding="utf-8")
        before = (self.workspace / ".env").read_bytes()
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "HOST_FLOOR_INVALID")
        self.assert_failure_evidence("HOST_FLOOR_INVALID")
        self.assertEqual((self.workspace / ".env").read_bytes(), before)
        self.assert_no_mutations()
        (self.workspace / ".env").unlink()
        self.reset_install_state()
        self.executor.preexisting_project = True
        before_project = self.project_snapshot()
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "LIVE_IDENTITY_INVALID")
        self.assert_failure_evidence("LIVE_IDENTITY_INVALID")
        self.assertTrue(any("docker" in call and "ps" in call for call in self.executor.calls))
        self.assertTrue(any("docker" in call and "volume" in call for call in self.executor.calls))
        self.assertEqual(self.project_snapshot(), before_project)
        self.assert_no_mutations()

    def test_invalid_answers_never_publish_environment(self):
        result = self.run_admitted()
        invalid = next(event for event in self.executor.events if event[0] == "installer-invalid")
        self.assertEqual(invalid[1], "False")
        self.assertLess(self.executor.events.index(invalid),
                        next(i for i, event in enumerate(self.executor.events)
                             if event[0] == "installer-valid"))
        invalid_result = next(item for item in self.executor.results
                               if "install-selfhost.sh" in " ".join(item[0])
                               and item[2] != 0)
        self.assertIn("invalid", invalid_result[4])
        first_publication = next(event for event in self.executor.events
                                 if event[0] == "installer-valid")
        published, published_mode = first_publication[1], first_publication[2]
        self.assertIn(b"DOMAIN=ci-selfhost.example.test\n", published)
        self.assertEqual(published_mode, 0o600)
        installer_calls = [item for item in self.executor.results
                           if "install-selfhost.sh" in " ".join(item[0])]
        self.assertGreaterEqual(len(installer_calls), 3,
                                "runner must rerun the shipped installer after successful publication")
        self.assertNotEqual(installer_calls[-1][2], 0)
        self.assertIn("selfhost-storage: existing installation refused", installer_calls[-1][4])
        rerun = next(event for event in self.executor.events if event[0] == "installer-rerun")
        self.assertEqual(rerun[1], published)
        self.assertEqual(rerun[2], published_mode)
        self.assertEqual(rerun[3], published)
        self.assertEqual(rerun[4], published_mode)
        rerun_at = self.executor.events.index(rerun)
        first_up = next(i for i, event in enumerate(self.executor.events)
                        if event[0] == "wrapper" and event[1] == "up")
        self.assertLess(rerun_at, first_up)
        self.assertEqual(result["public_checks"], "UNVERIFIED")

        self.executor = _OwnedExecutor(self.workspace)
        self.executor.invalid_answer_zero = True
        self.reset_install_state()
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "GUARD_INSTALL_INVALID")
        self.assertFalse((self.workspace / ".env").exists())
        self.assert_failure_evidence("GUARD_INSTALL_INVALID")

    def test_observation_accepts_optional_health_only_for_unchecked_services(self):
        for omit_health in (False, True):
            with self.subTest(omit_health=omit_health):
                self.executor = _OwnedExecutor(self.workspace)
                self.reset_install_state()
                original = self.executor.docker_inspect_fixture

                def inspect_rows():
                    rows = original()
                    for row in rows:
                        service = row["Config"]["Labels"]["com.docker.compose.service"]
                        if service in {"minio", "ops-agent"}:
                            if omit_health:
                                row["State"].pop("Health", None)
                            else:
                                row["State"]["Health"] = None
                    return rows

                with mock.patch.object(self.executor, "docker_inspect_fixture", side_effect=inspect_rows):
                    result = self.run_admitted()
                self.assertTrue(any(check["stage"] == "observe" and
                                    check["assertion"] == "selected_stack" and check["status"] == "PASS"
                                    for check in result["checks"]))

        for health in (None, {"Status": "starting"}, {"Status": "unhealthy"}):
            with self.subTest(required_health=health):
                self.executor = _OwnedExecutor(self.workspace)
                self.reset_install_state()
                original = self.executor.docker_inspect_fixture

                def invalid_rows():
                    rows = original()
                    for row in rows:
                        if row["Config"]["Labels"]["com.docker.compose.service"] == "control-plane":
                            row["State"]["Health"] = health
                    return rows

                with mock.patch.object(self.executor, "docker_inspect_fixture", side_effect=invalid_rows):
                    with self.assertRaises(acceptance.AcceptanceError) as caught:
                        self.run_admitted()
                self.assertEqual(caught.exception.code, "LIVE_STATE_INVALID")
                record = self.assert_failure_evidence("LIVE_STATE_INVALID")
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                                 [{"stage": "observe", "assertion": "live_control_plane_state",
                                   "status": "FAIL", "code": "LIVE_STATE_INVALID"}])
                self.assertFalse(any(event[0] == "route" for event in self.executor.events))

    def test_observed_state_attribution_never_exports_unvalidated_identity(self):
        fixed_assertions = {"postgres": "live_postgres_state", "redis": "live_redis_state",
                            "nats": "live_nats_state", "coturn": "live_coturn_state",
                            "minio": "live_minio_state", "ops-agent": "live_ops_agent_state",
                            "control-plane": "live_control_plane_state", "media-plane": "live_media_plane_state"}
        cases = [(service, "LIVE_STATE_INVALID", assertion)
                 for service, assertion in fixed_assertions.items()]
        cases += [("invalid-service", "LIVE_SERVICE_SET_INVALID", "selected_stack"),
                  ("invalid-name", "LIVE_IDENTITY_INVALID", "selected_stack")]
        for variant, code, assertion in cases:
            with self.subTest(variant=variant):
                self.executor = _OwnedExecutor(self.workspace)
                self.reset_install_state()
                original = self.executor.docker_inspect_fixture

                def changed_rows():
                    rows = original()
                    row = next(row for row in rows if row["Config"]["Labels"]["com.docker.compose.service"] ==
                               (variant if variant in fixed_assertions else "control-plane"))
                    if variant in fixed_assertions and variant != "ops-agent":
                        row["State"]["Status"] = "PRIVATE_STATE_SENTINEL"
                    elif variant == "ops-agent":
                        row["State"]["ExitCode"] = 19
                    elif variant == "invalid-service":
                        row["Config"]["Labels"]["com.docker.compose.service"] = "PRIVATE_SERVICE_SENTINEL"
                    else:
                        row["Name"] = "/PRIVATE_CONTAINER_NAME_SENTINEL"
                    return rows

                with mock.patch.object(self.executor, "docker_inspect_fixture", side_effect=changed_rows):
                    with self.assertRaises(acceptance.AcceptanceError) as caught:
                        self.run_admitted()
                self.assertEqual(caught.exception.code, code)
                self.assertIsNone(caught.exception.returncode)
                record = self.assert_failure_evidence(code)
                self.assertEqual(set(record), {"schema", "candidate_sha", "run_id", "runner_os",
                                                "tool_versions", "local_only", "public_checks", "checks"})
                expected_failures = [{"stage": "observe", "assertion": assertion, "status": "FAIL", "code": code}]
                if variant == "coturn":
                    expected_failures.insert(0, {"stage": "observe", "assertion": "live_coturn_not_running",
                                                 "status": "FAIL", "code": code})
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"], expected_failures)
                for sentinel in ("PRIVATE_STATE_SENTINEL", "PRIVATE_SERVICE_SENTINEL", "PRIVATE_CONTAINER_NAME_SENTINEL"):
                    self.assertNotIn(sentinel, json.dumps(record))
                self.assertFalse(any(event[0] == "route" for event in self.executor.events))

    def test_coturn_starting_converges_with_bounded_owned_probes(self):
        scenarios = [
            ("converges", "starting", None, 0, None),
            ("deadline", "starting", None, 0, "live_coturn_starting"),
            ("slow inspect", "starting", None, 181, "live_coturn_starting"),
            ("late healthy", "starting", None, 181, "live_coturn_starting"),
            ("retry command failure", "starting", None, 0, None),
            ("unhealthy", "unhealthy", None, 0, "live_coturn_unhealthy"),
            ("invalid health", "PRIVATE_HEALTH_SENTINEL", None, 0, "live_coturn_health_invalid"),
            ("not running", "healthy", "exited", 0, "live_coturn_not_running"),
            ("other service", "healthy", "other-service", 0, None),
            ("invalid identity", "healthy", "identity", 0, None),
            ("starting plus other unhealthy", "starting", "other-service", 0, None),
            ("starting plus missing port", "starting", "missing-port", 0, None),
            ("starting plus extra port", "starting", "extra-port", 0, None),
        ]
        for label, health, mutation, delay, terminal in scenarios:
            with self.subTest(scenario=label):
                self.executor = _OwnedExecutor(self.workspace)
                self.reset_install_state()
                now = [0.0]
                probes = []
                sleeps = []
                original = self.executor.run
                observing = [False]
                original_observe = acceptance.AcceptanceRunner._observe_live

                def observe(runner, rendered):
                    observing[0] = True
                    try:
                        return original_observe(runner, rendered)
                    finally:
                        observing[0] = False

                def sleep(seconds):
                    self.assertGreater(seconds, 0)
                    self.assertLessEqual(seconds, 2)
                    self.assertLessEqual(seconds, 180 - now[0])
                    sleeps.append(seconds)
                    now[0] += seconds

                clock = mock.Mock()
                clock.monotonic.side_effect = lambda: now[0]
                clock.sleep.side_effect = sleep

                def child(argv, timeout, input_text=None):
                    result = original(argv, timeout, input_text)
                    if (observing[0] and argv[:2] == ["docker", "inspect"] and
                            "--format" in argv and
                            len(argv[argv.index("--format") + 2:]) == 8 and
                            set(argv[argv.index("--format") + 2:]) == set(CONTAINER_NAMES.values())):
                        if label != "converges":
                            self.assertLess(now[0], 180, "no probe may start after its deadline")
                        self.assertLessEqual(timeout, 8)
                        self.assertGreater(timeout, 0)
                        self.assertLessEqual(timeout, 180 - now[0])
                        probes.append(now[0])
                        if label == "retry command failure" and len(probes) == 2:
                            return acceptance.CommandResult(43, "", "PRIVATE_RETRY_COMMAND_SENTINEL")
                        now[0] += delay if label != "late healthy" or len(probes) >= 2 else 0
                        rows = json.loads(result.stdout)
                        coturn = next(row for row in rows if row["Config"]["Labels"]["com.docker.compose.service"] == "coturn")
                        coturn["State"]["Health"] = {"Status": "healthy" if label == "converges" and len(probes) >= 3 else health}
                        if label == "late healthy" and len(probes) >= 2:
                            coturn["State"]["Health"] = {"Status": "healthy"}
                        if mutation == "exited":
                            coturn["State"].update(Status="exited", Running=False)
                        elif mutation == "other-service":
                            row = next(row for row in rows if row["Config"]["Labels"]["com.docker.compose.service"] == "control-plane")
                            row["State"]["Health"] = {"Status": "unhealthy"}
                        elif mutation == "identity":
                            coturn["Name"] = "/PRIVATE_IDENTITY_SENTINEL"
                        elif mutation in {"missing-port", "extra-port"}:
                            row = next(row for row in rows if row["Config"]["Labels"]["com.docker.compose.service"] == "control-plane")
                            if mutation == "missing-port":
                                row["NetworkSettings"]["Ports"].pop("8080/tcp")
                            else:
                                row["NetworkSettings"]["Ports"]["9999/tcp"] = [{"HostIp": "127.0.0.1", "HostPort": "9999"}]
                        return acceptance.CommandResult(0, json.dumps(rows), "")
                    if any(event[0] == "route" for event in self.executor.events) and label == "converges":
                        self.assertGreaterEqual(len(probes), 3)
                    return result

                with mock.patch.object(acceptance, "time", clock, create=True), \
                     mock.patch.object(acceptance.AcceptanceRunner, "_observe_live", new=observe), \
                     mock.patch.object(self.executor, "run", side_effect=child):
                    if label == "converges":
                        result = self.run_admitted()
                        self.assertTrue(any(check["stage"] == "observe" and check["assertion"] == "selected_stack" and
                                            check["status"] == "PASS" for check in result["checks"]))
                    else:
                        with self.assertRaises(acceptance.AcceptanceError) as caught:
                            self.run_admitted()
                        code = {"identity": "LIVE_IDENTITY_INVALID", "missing-port": "LIVE_PORT_MISSING",
                                "extra-port": "LIVE_PORT_EXTRA"}.get(mutation, "LIVE_STATE_INVALID")
                        if label == "retry command failure":
                            code = "COMMAND_FAILED"
                        self.assertEqual(caught.exception.code, code)
                        self.assertEqual(caught.exception.returncode, 43 if label == "retry command failure" else None)
                        result = self.assert_failure_evidence(code)
                        failures = [check["assertion"] for check in result["checks"] if check["status"] == "FAIL"]
                        if terminal:
                            self.assertIn("live_coturn_state", failures)
                            self.assertIn(terminal, failures)
                        else:
                            self.assertEqual(len(failures), 1)
                            if label == "retry command failure":
                                self.assertEqual(failures, ["live_coturn_state"])
                        self.assertFalse(any(check["stage"] == "observe" and check["status"] == "PASS"
                                             for check in result["checks"]))
                        self.assertFalse(any(event[0] == "route" for event in self.executor.events))
                        self.assertNotIn("PRIVATE_HEALTH_SENTINEL", json.dumps(result))
                        self.assertNotIn("PRIVATE_IDENTITY_SENTINEL", json.dumps(result))
                        self.assertNotIn("PRIVATE_RETRY_COMMAND_SENTINEL", json.dumps(result))
                if label == "converges":
                    self.assertGreaterEqual(len(probes), 3)
                    self.assertGreaterEqual(len(sleeps), 2)
                elif label == "deadline":
                    self.assertEqual(now[0], 180)
                    self.assertGreater(len(probes), 1)
                elif label == "late healthy":
                    self.assertEqual(len(probes), 2)
                    self.assertEqual(len(sleeps), 1)
                    self.assertGreater(now[0], 180)
                elif label == "retry command failure":
                    self.assertEqual(len(probes), 2)
                    self.assertEqual(len(sleeps), 1)
                else:
                    self.assertEqual(len(probes), 1)
                    self.assertEqual(sleeps, [])

    def test_exact_compose_selection_live_validation_and_host_identity(self):
        result = self.run_admitted()
        evidence = json.loads((self.evidence / "evidence.json").read_text(encoding="utf-8"))
        self.assertEqual(evidence, result)
        observe_checks = [check for check in evidence["checks"]
                          if check["stage"] == "observe"]
        for assertion in ("delivered_tls_metadata", "served_tls_leaf"):
            with self.subTest(tls_assertion=assertion):
                matches = [check for check in observe_checks
                           if check["assertion"] == assertion]
                self.assertEqual(matches, [{"stage": "observe", "assertion": assertion,
                                            "status": "PASS", "code": "OK"}])
        configs = [event for event in self.executor.events if event[0] == "compose-config"]
        self.assertTrue(any(event[1] == ("docker-compose.yml", "docker-compose.production.yml")
                            and event[2] and event[4] for event in configs))
        selected = next(event for event in configs if event[1] ==
                        ("docker-compose.yml", "docker-compose.production.yml",
                         "docker-compose.selfhost.yml"))
        self.assertFalse(selected[2])
        self.assertTrue(selected[3] and selected[4])
        selected_at = self.executor.events.index(selected)
        first_up = next(i for i, event in enumerate(self.executor.events)
                        if event[0] == "wrapper" and event[1] == "up")
        self.assertLess(selected_at, first_up)
        identity_event = next(event for event in self.executor.events
                              if event[0] == "project-identity")
        first_project_scan = next(i for i, event in enumerate(self.executor.events)
                                  if event[0] == "project-state-scan")
        self.assertEqual(identity_event[1], PROJECT)
        self.assertEqual(identity_event[2],
                         ("docker", "compose", "--project-directory", str(self.workspace),
                          "--env-file", "/dev/null", "-f", "-", "config", "--format", "json"))
        self.assertEqual(identity_event[3], "services:\n  identity:\n    image: scratch\n")
        self.assertLess(self.executor.events.index(identity_event), first_project_scan)
        rendered_identity = next(event for event in self.executor.events
                                 if event[0] == "selected-model")
        self.assertEqual(rendered_identity, ("selected-model", PROJECT, PROJECT))
        live_inspect = next(result for result in self.executor.results
                            if result[0][:2] == ["docker", "inspect"] and result[2] == 0)
        live_rows = json.loads(live_inspect[3])
        self.assertTrue(all(row["Config"]["Labels"]["com.docker.compose.project"] == PROJECT
                            for row in live_rows))
        for call in self.executor.calls:
            if "docker" in call and "compose" in call and "exec" in call:
                self.assertEqual(call[call.index("-p") + 1], PROJECT)
        for route in ("control-local", "control-ready", "media-local", "media-ready",
                      "nginx-api", "nginx-owned", "nginx-media"):
            self.assertTrue(any(event[0] == "route" and event[1] == route
                                for event in self.executor.events), route)
        activation = [event[0] for event in self.executor.events
                      if event[0] in {"nginx-vhost-install", "nginx-enable",
                                      "nginx-config-test", "nginx-start", "nginx-active", "nginx-reload"}]
        self.assertEqual(activation, ["nginx-vhost-install", "nginx-enable",
                                      "nginx-config-test", "nginx-start", "nginx-active", "nginx-reload"])
        first_up = next(i for i, event in enumerate(self.executor.events)
                        if event[0] == "wrapper" and event[1] == "up")
        self.assertLess(next(i for i, event in enumerate(self.executor.events)
                             if event[0] == "nginx-reload"), first_up)
        first_https = next(i for i, event in enumerate(self.executor.events)
                           if event[0] == "route" and event[2].startswith("https://"))
        self.assertLess(max(i for i, event in enumerate(self.executor.events)
                            if event[0] in {"nginx-vhost-install", "nginx-enable",
                                            "nginx-config-test", "nginx-start", "nginx-active", "nginx-reload"}),
                        first_https)
        media_response = next(item[3] for item in self.executor.results
                             if "curl" in item[0] and "socket.io" in " ".join(item[0]))
        open_packet = media_response.splitlines()[1]
        self.assertTrue(open_packet.startswith("0"))
        engineio = json.loads(open_packet[1:])
        self.assertRegex(engineio["sid"], r"^fixture-owned-session-[1-9][0-9]*$")
        self.assertIn("websocket", engineio["upgrades"])
        self.assertEqual(engineio["pingInterval"], 25000)
        self.assertEqual(engineio["pingTimeout"], 20000)
        self.assertGreater(engineio["maxPayload"], 0)
        select_calls = [call for call in self.executor.calls
                        if "psql" in call and any("SELECT id FROM selfhost_acceptance_2505" in arg
                                                   for arg in call)]
        self.assertGreaterEqual(len(select_calls), 3)
        for call in select_calls:
            self.assertIn("-t", call)
            self.assertIn("-A", call)
        self.assertTrue(any("docker" in call and "info" in call for call in self.executor.calls))
        self.assertTrue(any("docker" in call and "context" in call and "inspect" in call
                            for call in self.executor.calls))
        self.assertTrue(any("rev-parse" in call and "HEAD" in call for call in self.executor.calls))
        log_sample = next(result for result in self.executor.results
                          if any("concord-selfhost" in arg for arg in result[0])
                          and "logs" in result[0]
                          and "not-a-service" not in result[0])
        self.assertIn("--tail=20", log_sample[0])
        self.assertEqual(log_sample[2], 0)
        self.assertTrue(log_sample[3].strip())
        source_fingerprint = next(event for event in self.executor.events
                                  if event[0] == "leaf-fingerprint" and event[1] == "source")
        served_fingerprint = next(event for event in self.executor.events
                                  if event[0] == "leaf-fingerprint" and event[1] == "served")
        self.assertEqual(source_fingerprint[2], served_fingerprint[2])
        self.assertEqual(source_fingerprint[4], None)
        self.assertIn("-in", source_fingerprint[3])
        self.assertEqual(source_fingerprint[3][source_fingerprint[3].index("-in") + 1],
                         "/etc/letsencrypt/live/ci-selfhost.example.test/fullchain.pem")
        self.assertEqual(served_fingerprint[4], "SYNTHETIC_SERVED_CERTIFICATE_2505\n")
        metadata = next(event for event in self.executor.events
                        if event[0] == "delivered-tls-metadata")
        self.assertEqual(metadata[1], None)
        self.assertEqual(metadata[2], ("stat", "-c", "%F|%u|%g|%a|%s",
                                       "/opt/concord/certs",
                                       "/opt/concord/certs/coturn",
                                       "/opt/concord/certs/coturn/cert.pem",
                                       "/opt/concord/certs/coturn/key.pem"))
        self.assertEqual(metadata[3],
                         "directory|0|0|711|4096\ndirectory|0|0|711|4096\n"
                         "regular file|0|0|444|1024\nregular file|65534|65533|400|2048\n")
        self.assertLess(self.executor.events.index(metadata),
                        next(index for index, event in enumerate(self.executor.events)
                             if event[0] == "route"))
        probe = next(event for event in self.executor.events if event[0] == "served-leaf-probe")
        probe_argv = probe[1]
        self.assertEqual(probe_argv[probe_argv.index("-connect") + 1], "127.0.0.1:5349")
        self.assertEqual(probe_argv[probe_argv.index("-servername") + 1],
                         "turn.ci-selfhost.example.test")
        self.assertEqual(probe_argv[probe_argv.index("-verify_hostname") + 1],
                         "turn.ci-selfhost.example.test")
        self.assertEqual(probe_argv[probe_argv.index("-CAfile") + 1],
                         "/etc/letsencrypt/live/ci-selfhost.example.test/fullchain.pem")
        self.assertTrue(any("s_client" in call for call in self.executor.calls),
                        "runner must probe the certificate actually served on the local endpoint")
        self.assertTrue(any("-fingerprint" in call and "x509" in call for call in self.executor.calls))

        for variant in ("missing-control-plane", "nonzero-exited-agent", "sha-mismatch",
                        "served-leaf-mismatch", "remote-daemon", "non-linux-engine",
                        "bad-engineio", "bad-nginx-health", "bad-control-ready",
                        "bad-media-ready", "wrong-rendered-project", "wrong-live-project",
                        "rendered-model-invalid", "rendered-service-invalid",
                        "tls-root-owner", "tls-root-mode", "tls-root-not-directory",
                        "tls-root-empty", "tls-coturn-owner",
                        "tls-cert-owner", "tls-cert-mode", "tls-key-owner", "tls-key-mode",
                        "tls-not-regular", "tls-empty", "tls-malformed"):
            with self.subTest(variant=variant):
                self.reset_install_state()
                self.executor = _OwnedExecutor(self.workspace)
                if variant == "missing-control-plane":
                    self.executor.live_variant = variant
                elif variant == "nonzero-exited-agent":
                    self.executor.ops_agent_exit_code = 1
                elif variant == "sha-mismatch":
                    self.executor.sha_mismatch = True
                elif variant == "served-leaf-mismatch":
                    self.executor.served_leaf_mismatch = True
                elif variant == "remote-daemon":
                    self.executor.daemon_endpoint = "tcp://192.0.2.55:2376"
                elif variant == "non-linux-engine":
                    self.executor.engine_os = "windows"
                elif variant == "bad-engineio":
                    self.executor.route_variant = "nginx-media"
                elif variant == "bad-nginx-health":
                    self.executor.route_variant = "nginx-owned"
                elif variant == "bad-control-ready":
                    self.executor.route_variant = "control-ready"
                elif variant == "bad-media-ready":
                    self.executor.route_variant = "media-ready"
                elif variant == "wrong-rendered-project":
                    self.executor.rendered_project = "foreign-rendered-project"
                elif variant == "wrong-live-project":
                    self.executor.live_project = "foreign-live-project"
                elif variant == "rendered-model-invalid":
                    self.executor.rendered_variant = "model-invalid"
                elif variant == "rendered-service-invalid":
                    self.executor.rendered_variant = "service-invalid"
                elif variant.startswith("tls-"):
                    self.executor.delivered_tls_metadata_variant = variant[4:]
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                expected = {
                    "missing-control-plane": "LIVE_PORT_MISSING",
                    "nonzero-exited-agent": "LIVE_STATE_INVALID",
                    "sha-mismatch": "HOST_FLOOR_INVALID",
                    "remote-daemon": "HOST_FLOOR_INVALID",
                    "non-linux-engine": "HOST_FLOOR_INVALID",
                    "served-leaf-mismatch": "TLS_LEAF_INVALID",
                    "bad-engineio": "ROUTE_INVALID",
                    "bad-nginx-health": "ROUTE_INVALID",
                    "bad-control-ready": "ROUTE_INVALID",
                    "bad-media-ready": "ROUTE_INVALID",
                    "wrong-rendered-project": "RENDERED_IDENTITY_INVALID",
                    "wrong-live-project": "LIVE_IDENTITY_INVALID",
                    "rendered-model-invalid": "RENDERED_MODEL_INVALID",
                    "rendered-service-invalid": "RENDERED_SERVICE_INVALID",
                    "tls-root-owner": "TLS_LEAF_INVALID",
                    "tls-root-mode": "TLS_LEAF_INVALID",
                    "tls-root-not-directory": "TLS_LEAF_INVALID",
                    "tls-root-empty": "TLS_LEAF_INVALID",
                    "tls-coturn-owner": "TLS_LEAF_INVALID",
                    "tls-cert-owner": "TLS_LEAF_INVALID",
                    "tls-cert-mode": "TLS_LEAF_INVALID",
                    "tls-key-owner": "TLS_LEAF_INVALID",
                    "tls-key-mode": "TLS_LEAF_INVALID",
                    "tls-not-regular": "TLS_LEAF_INVALID",
                    "tls-empty": "TLS_LEAF_INVALID",
                    "tls-malformed": "TLS_LEAF_INVALID",
                }[variant]
                self.assertEqual(caught.exception.code, expected)
                failure_record = self.assert_failure_evidence(caught.exception.code)
                if variant.startswith("tls-"):
                    self.assertTrue(any(event[0] == "delivered-tls-metadata"
                                        for event in self.executor.events),
                                    "metadata mutants must reach the independent delivered-file check")
                    self.assertTrue(any(check["stage"] == "observe" and
                                        check["assertion"] == "delivered_tls_metadata" and
                                        check["status"] == "FAIL" and
                                        check["code"] == "TLS_LEAF_INVALID"
                                        for check in failure_record["checks"]))
                    self.assertFalse(any(check["assertion"] in
                                         {"delivered_tls_metadata", "served_tls_leaf"} and
                                         check["status"] == "PASS"
                                         for check in failure_record["checks"]))
                elif variant == "served-leaf-mismatch":
                    self.assertTrue(any(check["stage"] == "observe" and
                                        check["assertion"] == "delivered_tls_metadata" and
                                        check["status"] == "PASS" and check["code"] == "OK"
                                        for check in failure_record["checks"]))
                    self.assertTrue(any(check["stage"] == "observe" and
                                        check["assertion"] == "served_tls_leaf" and
                                        check["status"] == "FAIL" and
                                        check["code"] == "TLS_LEAF_INVALID"
                                        for check in failure_record["checks"]))
                    self.assertFalse(any(check["assertion"] == "served_tls_leaf" and
                                         check["status"] == "PASS"
                                         for check in failure_record["checks"]))
                if variant in {"remote-daemon", "non-linux-engine"}:
                    self.assert_no_mutations()
                    self.assertFalse((self.workspace / ".env").exists())

    def test_guarded_missing_log_and_tls_do_not_start(self):
        result = self.run_admitted()
        self.assertEqual(result["public_checks"], "UNVERIFIED")
        self.assertEqual(self.executor.guard_count, {"log": 1, "tls": 1})
        self.assertEqual(self.executor.stack, "running")
        for guard, provision in (("guard-log", "provision-log"),
                                 ("guard-tls", "provision-tls")):
            refusal = next(event for event in self.executor.events if event[0] == guard)
            self.assertEqual(refusal[1], "1")
            expected_reason = ("security-event log boundary missing: /var/log/concord-security"
                               if guard == "guard-log" else "self-host TLS material refused")
            self.assertEqual(refusal[2], expected_reason)
            self.assertEqual(refusal[3], refusal[4], "expected refusal must not mutate project state")
            guard_at = next(i for i, event in enumerate(self.executor.events) if event[0] == guard)
            provision_at = next(i for i, event in enumerate(self.executor.events)
                                if event[0] == provision)
            self.assertLess(guard_at, provision_at)
        evidence = json.loads((self.evidence / "evidence.json").read_text(encoding="utf-8"))
        self.assertTrue(evidence["checks"])
        self.assertTrue(any(check["status"] == "PASS" for check in evidence["checks"]))

        for variant in ("log-zero", "log-wrong", "log-mutate",
                        "tls-zero", "tls-wrong", "tls-mutate"):
            with self.subTest(variant=variant):
                self.reset_install_state()
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.malformed_guard = variant
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                expected_code = "GUARD_LOG_INVALID" if variant.startswith("log-") else "GUARD_TLS_INVALID"
                self.assertEqual(caught.exception.code, expected_code)
                self.assert_failure_evidence(caught.exception.code)

    def test_child_failure_is_not_completion_and_redacts_output(self):
        self.executor.fail_startup = True
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "COMMAND_FAILED")
        self.assertEqual(caught.exception.returncode, 37)
        self.assertNotIn(self.executor.failure_sentinel, str(caught.exception))
        record = self.assert_failure_evidence("COMMAND_FAILED")
        self.assertTrue(any(check["stage"] == "observe" and
                            check["assertion"] == "selected_stack" and
                            check["status"] == "FAIL" and
                            check["code"] == "COMMAND_FAILED"
                            for check in record["checks"]))
        self.assertFalse(any(check["stage"] == "observe" and
                             check["assertion"] == "selected_stack" and
                             check["status"] == "PASS"
                             for check in record["checks"]))
        self.assertNotIn(self.executor.failure_sentinel,
                         (self.evidence / "evidence.json").read_text(encoding="utf-8"))

    def test_health_after_stop(self):
        self.run_admitted()
        wrapper_results = []
        for index, (argv, _input, returncode, stdout, stderr) in enumerate(self.executor.results):
            if "concord-selfhost" in " ".join(argv):
                token = next((item for item in argv
                              if item in {"up", "health", "status", "logs", "stop", "down"}), "")
                wrapper_results.append((index, token, returncode, stdout + stderr))
        stop_at = next(index for index, verb, _rc, _text in wrapper_results if verb == "stop")
        self.assertTrue(any(verb == "health" and index < stop_at
                            for index, verb, _rc, _text in wrapper_results))
        health_at, _verb, health_rc, health_text = next(item for item in wrapper_results
                                                        if item[1] == "health" and item[0] > stop_at)
        self.assertGreater(health_at, stop_at)
        self.assertNotEqual(health_rc, 0)
        self.assertIn("unhealthy", health_text)

        self.executor = _OwnedExecutor(self.workspace)
        self.reset_install_state()
        self.executor.zero_lifecycle = "health-after-stop"
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "LIFECYCLE_HEALTH_INVALID")
        self.assert_failure_evidence(caught.exception.code)

    def test_guard_refusals_require_completed_expected_diagnostics(self):
        paths = [
            ("installer-invalid", "installer: invalid domain", "GUARD_INSTALL_INVALID", "render", "port_posture"),
            ("installer-rerun", "selfhost-storage: existing installation refused", "GUARD_INSTALL_INVALID", "render", "port_posture"),
            ("guard-log", "security-event log boundary missing: /var/log/concord-security", "GUARD_LOG_INVALID", "render", "local_only"),
            ("guard-tls", "self-host TLS material refused", "GUARD_TLS_INVALID", "render", "coturn_mount"),
            ("wrapper-logs", "selfhost-storage: unknown log service", "LIFECYCLE_LOGS_INVALID", "lifecycle", "evidence_safety"),
            ("wrapper-unknown", "unknown verb: unknown", "LIFECYCLE_UNKNOWN_INVALID", "lifecycle", "evidence_safety"),
        ]
        cases = [(event, acceptance.CommandResult(returncode, "", diagnostic + "\nPRIVATE_REFUSAL_SENTINEL"), code, stage, assertion)
                 for event, diagnostic, code, stage, assertion in paths
                 for returncode in (124, -15, -9)]
        cases += [("installer-invalid", acceptance.CommandResult(1, "", reason), "GUARD_INSTALL_INVALID", "render", "port_posture")
                  for reason in ("", "installer: unrelated invalid input", "PRIVATE_REFUSAL_SENTINEL")]
        for event, child_result, code, stage, assertion in cases:
            with self.subTest(event=event, diagnostic=child_result.stderr):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.refusal_result_override = (event, child_result)
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, code)
                self.assertEqual(caught.exception.returncode, child_result.returncode)
                record = self.assert_failure_evidence(code)
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                                 [{"stage": stage, "assertion": assertion, "status": "FAIL", "code": code}])
                self.assertFalse(any(check["assertion"] == "plain_down_preserves_volumes" and check["status"] == "PASS"
                                     for check in record["checks"]))
                self.assertNotIn("PRIVATE_REFUSAL_SENTINEL", json.dumps(record))
                self.assertTrue(any(item[0] == event for item in self.executor.events))
                if stage == "render":
                    self.assertFalse(any(item == ("wrapper", "up") for item in self.executor.events))
                    self.assertFalse(any(item[0] == "route" for item in self.executor.events))

    def test_timed_out_stopped_health_is_not_completed_refusal(self):
        for returncode in (124, -15, -9):
            with self.subTest(returncode=returncode):
                self.executor = _OwnedExecutor(self.workspace)
                self.reset_install_state()
                self.executor.stopped_health_timeout = returncode
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "LIFECYCLE_HEALTH_INVALID")
                self.assertEqual(caught.exception.returncode, returncode)
                record = self.assert_failure_evidence("LIFECYCLE_HEALTH_INVALID")
                self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                                 [{"stage": "lifecycle", "assertion": "stopped_health",
                                   "status": "FAIL", "code": "LIFECYCLE_HEALTH_INVALID"}])
                self.assertFalse(any(check["assertion"] in {"stopped_health", "plain_down_preserves_volumes"} and
                                     check["status"] == "PASS" for check in record["checks"]))
                self.assertNotIn(self.executor.failure_sentinel, json.dumps(record))
                self.assertTrue(any(event == ("wrapper", "stop") for event in self.executor.events))
                self.assertFalse(any(event == ("wrapper", "down") for event in self.executor.events))
                self.assertEqual(sum(event == ("wrapper", "up") for event in self.executor.events), 1)
                health_results = [item for item in self.executor.results
                                 if "concord-selfhost" in " ".join(item[0]) and "health" in item[0]]
                self.assertEqual([item[2] for item in health_results], [0, returncode])

    def test_unknown_verb_and_invalid_logs_service(self):
        self.run_admitted()
        sample = next(call for call in self.executor.calls
                      if "concord-selfhost" in " ".join(call) and "logs" in call and "postgres" in call)
        self.assertEqual(sample[-4:], ["logs", "--no-follow", "--tail=20", "postgres"])
        self.assertTrue(any(event[0] == "wrapper-unknown" and event[1] == "1"
                            and "unknown verb" in event[2] for event in self.executor.events))
        self.assertTrue(any(event[0] == "wrapper-logs" and event[1] == "1"
                            and event[2] == "selfhost-storage: unknown log service"
                            for event in self.executor.events))
        for refusal in ("unknown", "logs"):
            with self.subTest(refusal=refusal):
                self.reset_install_state()
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.zero_lifecycle = refusal
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                expected_code = ("LIFECYCLE_UNKNOWN_INVALID" if refusal == "unknown"
                                 else "LIFECYCLE_LOGS_INVALID")
                self.assertEqual(caught.exception.code, expected_code)
                self.assert_failure_evidence(caught.exception.code)

    def test_bounded_log_sample_requires_nonempty_output(self):
        self.executor.log_sample_timeout_empty = True
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "LIFECYCLE_LOGS_INVALID")
        self.assert_failure_evidence("LIFECYCLE_LOGS_INVALID")
        self.assertTrue(any(event == ("wrapper-log-sample", "124", "")
                            for event in self.executor.events))
        log_call = next(call for call in self.executor.calls
                        if any("concord-selfhost" in arg for arg in call) and "logs" in call
                        and "not-a-service" not in call)
        self.assertIn("--tail=20", log_call)
        self.assertIn("--no-follow", log_call)

    def test_log_sample_requires_completed_stdout_and_specific_service_refusal(self):
        for child in (acceptance.CommandResult(124, "owned partial", ""),
                      acceptance.CommandResult(-15, "owned partial", ""),
                      acceptance.CommandResult(1, "owned partial", ""),
                      acceptance.CommandResult(0, "", "PRIVATE_STDERR_ONLY")):
            with self.subTest(returncode=child.returncode):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.log_sample_result = child
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "LIFECYCLE_LOGS_INVALID")
                self.assertEqual(caught.exception.returncode, child.returncode)
                record = self.assert_failure_evidence("LIFECYCLE_LOGS_INVALID")
                self.assertNotIn("PRIVATE_STDERR_ONLY", json.dumps(record))
                self.assertFalse(any(check["assertion"] == "plain_down_preserves_volumes" and check["status"] == "PASS"
                                     for check in record["checks"]))
        self.executor = _OwnedExecutor(self.workspace)
        self.executor.invalid_logs_diagnostic = "selfhost-storage: Compose command failed"
        self.reset_install_state()
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            self.run_admitted()
        self.assertEqual(caught.exception.code, "LIFECYCLE_LOGS_INVALID")

    def test_refusal_snapshots_ignore_age_order_but_detect_inventory_changes(self):
        self.executor.inventory_variant = "benign"
        result = self.run_admitted()
        self.assertEqual(result["public_checks"], "UNVERIFIED")
        self.assertTrue(any(check["assertion"] == "plain_down_preserves_volumes" and
                            check["status"] == "PASS" for check in result["checks"]))
        inventory_calls = [call for call in self.executor.calls if call[:2] == ["docker", "ps"]]
        self.assertTrue(inventory_calls)
        self.assertTrue(all("--no-trunc" in call for call in inventory_calls))
        for variant in ("id-change", "state-change", "health-change", "volume-add", "volume-delete",
                        "duplicate-id", "invalid-id", "invalid-state", "invalid-json"):
            with self.subTest(variant=variant):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.inventory_variant = variant
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                expected_code = ("LIVE_IDENTITY_INVALID" if variant in
                                 {"duplicate-id", "invalid-id", "invalid-state", "invalid-json"} else "LIFECYCLE_LOGS_INVALID")
                self.assertEqual(caught.exception.code, expected_code)
                record = self.assert_failure_evidence(expected_code)
                self.assertFalse(any(check["assertion"] == "plain_down_preserves_volumes" and
                                     check["status"] == "PASS" for check in record["checks"]))
                self.assertNotIn("PRIVATE INVALID ID", json.dumps(record))
                self.assertNotIn("PRIVATE_INVALID_STATE", json.dumps(record))

    def test_row_survives_each_restart(self):
        down_pass_observations = []
        original_record = acceptance.AcceptanceRunner._record

        def record(runner, stage, assertion, status, *args, **kwargs):
            if assertion == "plain_down_preserves_volumes" and status == "PASS":
                down_pass_observations.append(tuple(self.executor.events))
            return original_record(runner, stage, assertion, status, *args, **kwargs)

        with mock.patch.object(acceptance.AcceptanceRunner, "_record", new=record):
            result = self.run_admitted()
        self.assertEqual(sum(check["assertion"] == "plain_down_preserves_volumes" and
                             check["status"] == "PASS" for check in result["checks"]), 1)
        self.assertEqual(len(down_pass_observations), 1)
        self.assertEqual(sum(check["assertion"] == "stopped_health" and check["status"] == "PASS"
                             for check in result["checks"]), 1)
        observed_events = down_pass_observations[0]
        observed_down = next(i for i, event in enumerate(observed_events)
                             if event == ("wrapper", "down"))
        observed_restart = next(i for i, event in enumerate(observed_events)
                                if i > observed_down and event == ("wrapper", "up"))
        self.assertTrue(any(event == ("db-read", "owned-row")
                            for event in observed_events[observed_restart + 1:]))
        insert_at = next(i for i, event in enumerate(self.executor.events) if event[0] == "db" and event[1] == "insert")
        initial_read = next(i for i, event in enumerate(self.executor.events)
                            if event[0] == "db-read")
        first_stop = next(i for i, event in enumerate(self.executor.events)
                          if event[0] == "wrapper" and event[1] == "stop")
        self.assertLess(insert_at, initial_read)
        self.assertLess(initial_read, first_stop)
        for lifecycle_verb in ("stop", "down"):
            lifecycle_index = next(i for i, event in enumerate(self.executor.events)
                                   if event[0] == "wrapper" and event[1] == lifecycle_verb)
            subsequent = self.executor.events[lifecycle_index + 1:]
            restart_index = next(i for i, event in enumerate(subsequent)
                                 if event[0] == "wrapper" and event[1] == "up")
            readback = next(event for event in subsequent[restart_index + 1:]
                            if event[0] == "db-read")
            self.assertEqual(readback[1], "owned-row")
        for lifecycle_verb in ("stop", "down"):
            with self.subTest(lost_readback=lifecycle_verb):
                self.reset_install_state()
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.lost_readback = lifecycle_verb
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "DATA_SURVIVAL_INVALID")
                failure_record = self.assert_failure_evidence("DATA_SURVIVAL_INVALID")
                self.assertTrue(any(check["status"] == "FAIL" and
                                    check["assertion"] == "data_survives_restart"
                                    for check in failure_record["checks"]))
                self.assertFalse(any(check["status"] == "PASS" and
                                     check["assertion"] == "plain_down_preserves_volumes"
                                     for check in failure_record["checks"]))
                readbacks = [event[1] for event in self.executor.events if event[0] == "db-read"]
                wrappers = [event[1] for event in self.executor.events if event[0] == "wrapper"]
                self.assertEqual(readbacks[0], "owned-row", "initial baseline must succeed")
                if lifecycle_verb == "stop":
                    self.assertEqual(readbacks, ["owned-row", "missing"])
                    self.assertNotIn("down", wrappers)
                else:
                    self.assertEqual(readbacks, ["owned-row", "owned-row", "missing"])
                    self.assertEqual(wrappers.count("up"), 3)
                    self.assertIn("down", wrappers)

    def test_shutdown_budget_admits_slow_stop_and_down_without_expanding_other_calls(self):
        # A valid graceful shutdown can exceed the former 30-second child cap.
        # The fake models that budget boundary without sleeping or invoking Docker.
        self.executor.required_shutdown_seconds = 60
        clock = mock.Mock()
        clock.monotonic.return_value = 0.0
        with mock.patch.object(acceptance, "time", clock):
            result = self.run_admitted()
        shutdowns = [(verb, timeout) for verb, timeout in self.executor.wrapper_timeouts if verb in {"stop", "down"}]
        self.assertEqual(shutdowns, [("stop", 180), ("down", 180)])
        for verb, timeout in self.executor.wrapper_timeouts:
            if verb in {"health", "status", "unknown"}:
                self.assertEqual(timeout, 30)
            elif verb == "logs":
                self.assertIn(timeout, (8, 30))
        up_budgets = [timeout for verb, timeout in self.executor.wrapper_timeouts if verb == "up"]
        self.assertEqual(up_budgets, [30, 30, 2580, 2580, 2580])
        self.assertEqual([timeout for verb, timeout in self.executor.wrapper_timeouts if verb == "logs"], [8, 30])
        self.assertEqual(sum(check["assertion"] == "plain_down_preserves_volumes" and check["status"] == "PASS"
                             for check in result["checks"]), 1)

    def test_shutdown_failure_keeps_original_code_and_never_claims_retention(self):
        for verb in ("stop", "down"):
            for code in (1, 124):
                with self.subTest(verb=verb, returncode=code):
                    self.executor = _OwnedExecutor(self.workspace)
                    self.executor.shutdown_failure = (verb, code)
                    self.reset_install_state()
                    with self.assertRaises(acceptance.AcceptanceError) as caught:
                        self.run_admitted()
                    self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                    self.assertEqual(caught.exception.returncode, code)
                    record = self.assert_failure_evidence("COMMAND_FAILED")
                    expected = "stopped_health" if verb == "stop" else "plain_down_preserves_volumes"
                    self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                                     [{"stage": "lifecycle", "assertion": expected,
                                       "status": "FAIL", "code": "COMMAND_FAILED"}])
                    self.assertFalse(any(check["assertion"] == "plain_down_preserves_volumes" and
                                         check["status"] == "PASS" for check in record["checks"]))
                    self.assertNotIn(self.executor.failure_sentinel, json.dumps(record))

    def test_lifecycle_command_failure_never_claims_plain_down_survival(self):
        for verb, ordinal, expected in (("up", 2, "data_survives_restart"),
                                         ("down", 1, "plain_down_preserves_volumes"),
                                         ("up", 3, "data_survives_restart")):
            with self.subTest(verb=verb, ordinal=ordinal):
                self.executor = _OwnedExecutor(self.workspace)
                self.executor.lifecycle_command_failure = (verb, ordinal)
                self.reset_install_state()
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    self.run_admitted()
                self.assertEqual(caught.exception.code, "COMMAND_FAILED")
                self.assertEqual(caught.exception.returncode, 43)
                record = self.assert_failure_evidence("COMMAND_FAILED")
                self.assertTrue(any(check["status"] == "FAIL" and check["assertion"] == expected
                                    for check in record["checks"]))
                self.assertFalse(any(check["status"] == "PASS" and
                                     check["assertion"] == "plain_down_preserves_volumes"
                                     for check in record["checks"]))
                self.assertNotIn(self.executor.failure_sentinel, json.dumps(record))

    def test_no_volume_delete(self):
        result = self.run_admitted()
        commands = [call for call in self.executor.calls]
        for call in commands:
            if "down" in call:
                self.assertNotIn("--volumes", call)
                self.assertNotIn("-v", call)
        self.assertEqual(result["public_checks"], "UNVERIFIED")

    def test_artifact_only_regular_allowlisted_file(self):
        self.run_admitted()
        artifact = self.evidence / "evidence.json"
        self.assertTrue(artifact.is_file())
        record = json.loads(artifact.read_text(encoding="utf-8"))
        self.assertTrue(record["checks"])
        self.assertTrue(all(check["status"] == "PASS" for check in record["checks"]))
        extra = self.evidence / ".hidden"
        extra.write_text("owned unexpected file", encoding="utf-8")
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            acceptance.write_evidence(self.evidence, record)
        self.assertEqual(caught.exception.code, "EVIDENCE_PATH_INVALID")
        extra.unlink()
        self.assertFalse(artifact.is_symlink())
        self.assertEqual({entry.name for entry in self.evidence.iterdir()}, {"evidence.json"})
        linked = self.root / "linked-evidence"
        linked.symlink_to(self.evidence, target_is_directory=True)
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            acceptance.write_evidence(linked, record)
        self.assertEqual(caught.exception.code, "EVIDENCE_PATH_INVALID")



class InstallerFailureEvidenceTests(unittest.TestCase):
    """Owned failed-installer results reach the real installer/evidence seam.

    No child script or host collector executes here. These fixtures establish
    the diagnostic contract, not the cause of a hosted acceptance failure.
    """

    HOSTED = AcceptanceRunnerTests.HOSTED
    setUp = AcceptanceRunnerTests.setUp
    runner = AcceptanceRunnerTests.runner
    run_admitted = AcceptanceRunnerTests.run_admitted
    reset_install_state = AcceptanceRunnerTests.reset_install_state
    assert_failure_evidence = AcceptanceRunnerTests.assert_failure_evidence

    PRIVATE = "PRIVATE_INSTALLER_FAILURE_CANARY"
    GUARDS = (
        ("installer: UDP socket inventory unavailable or unparseable", "installer_udp_inventory"),
        ("installer: required UDP ports already in use", "installer_udp_busy"),
        ("selfhost-storage: existing installation refused", "installer_existing_installation"),
        ("selfhost-storage: installation discovery unavailable", "installer_discovery_unavailable"),
    )

    def assert_failed_installer(self, stdout, stderr, expected, *, returncode=37):
        self.reset_install_state()
        self.executor = _OwnedExecutor(self.workspace)
        self.executor.valid_installer_result = acceptance.CommandResult(returncode, stdout, stderr)
        output, error = io.StringIO(), io.StringIO()
        with mock.patch("sys.stdout", output), mock.patch("sys.stderr", error):
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                self.run_admitted()
        self.assertEqual(caught.exception.code, "COMMAND_FAILED")
        self.assertEqual(caught.exception.returncode, returncode)
        record = self.assert_failure_evidence("COMMAND_FAILED")
        self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                         [{"stage": "render", "assertion": expected,
                           "status": "FAIL", "code": "COMMAND_FAILED"}])
        self.assertIn({"stage": "render", "assertion": "evidence_safety",
                       "status": "PASS", "code": "OK"}, record["checks"])
        public = output.getvalue() + error.getvalue() + str(caught.exception)
        serialized = json.dumps(record)
        for private in (self.PRIVATE, str(self.workspace), str(self.evidence)):
            self.assertNotIn(private, public)
            self.assertNotIn(private, serialized)
        for diagnostic, _assertion in self.GUARDS:
            self.assertNotIn(diagnostic, public)
            self.assertNotIn(diagnostic, serialized)
        for raw in (stdout, stderr):
            if raw.strip():
                self.assertNotIn(raw, public)
                self.assertNotIn(raw, serialized)
        self.assertFalse((self.workspace / ".env").exists())
        self.assertFalse((self.workspace / ".env").is_symlink())
        self.assertEqual(self.executor.events.count(("installer-invalid", "False")), 1)
        self.assertEqual(self.executor.events.count(("installer-failure", returncode)), 1)
        self.assertFalse(any(event[0] in {"installer-valid", "installer-rerun", "route",
                                         "provision-log", "provision-tls", "wrapper"}
                             for event in self.executor.events))
        self.assertEqual(self.executor.guard_count, {"log": 0, "tls": 0})
        installer_calls = [entry for entry in self.executor.results
                           if "install-selfhost.sh" in " ".join(entry[0])]
        self.assertEqual(len(installer_calls), 2)
        self.assertEqual(Path(installer_calls[1][0][-1]).name, "literal-private.answers")
        self.assertEqual(installer_calls[1][2:], (returncode, stdout, stderr))

    def test_fixed_guards_classify_actual_failed_installer_and_preserve_child_status(self):
        for diagnostic, expected in self.GUARDS:
            for returncode in (1, 2, 37, 124, -9):
                with self.subTest(assertion=expected, returncode=returncode):
                    self.assert_failed_installer(self.PRIVATE, diagnostic, expected,
                                                 returncode=returncode)

    def test_whole_stderr_lines_allow_one_unique_category_without_exporting_surroundings(self):
        for diagnostic, expected in self.GUARDS:
            for stderr in (diagnostic + "\n", self.PRIVATE + "\n" + diagnostic + "\n" + self.PRIVATE,
                           diagnostic + "\n" + diagnostic + "\n"):
                with self.subTest(assertion=expected, lines=len(stderr.split("\n"))):
                    self.assert_failed_installer(self.PRIVATE, stderr, expected)

    def test_actual_colored_preflight_output_keeps_plain_udp_guard_classification(self):
        stdout = ("\x1b[0;34mRunning preflight (no files written yet)...\x1b[0m\n"
                  "\x1b[0;32m✓ OS ubuntu 24.04\x1b[0m\n"
                  "\x1b[0;32m✓ docker 29.8.2 + compose 2.24.5 + python3\x1b[0m\n"
                  "\x1b[0;32m✓ resources ok\x1b[0m\n"
                  "\x1b[0;32m✓ logrotate\x1b[0m\n")
        aborted = "\x1b[0;31mPreflight failed — aborting before any change.\x1b[0m\n"
        for diagnostic, expected in self.GUARDS[:2]:
            with self.subTest(assertion=expected):
                self.assert_failed_installer(stdout, diagnostic + "\n" + aborted, expected,
                                             returncode=2)

    def test_only_producer_sgr_sequences_are_allowed_in_ancillary_output(self):
        diagnostic, expected = self.GUARDS[0]
        for sgr in ("\x1b[0;31m", "\x1b[0;32m", "\x1b[1;33m", "\x1b[0;34m", "\x1b[0m"):
            ancillary = sgr + self.PRIVATE + "\x1b[0m\n"
            for channel in ("stdout", "stderr"):
                with self.subTest(sequence=repr(sgr), ancillary_channel=channel):
                    self.assert_failed_installer(ancillary if channel == "stdout" else self.PRIVATE,
                                                 diagnostic if channel == "stdout" else ancillary + diagnostic,
                                                 expected)
            with self.subTest(sequence=repr(sgr), colored_guard=True):
                self.assert_failed_installer(self.PRIVATE, sgr + diagnostic + "\x1b[0m", "port_posture")

    def test_unknown_malformed_and_substring_decoys_retain_port_posture(self):
        diagnostic = self.GUARDS[0][0]
        cases = [
            ("empty", "", ""),
            ("unknown private output", self.PRIVATE, self.PRIVATE),
            ("stdout only", diagnostic, self.PRIVATE),
            ("different fixed refusal", self.PRIVATE, "installer: environment publication refused"),
            ("private prefix", self.PRIVATE, self.PRIVATE + ": " + diagnostic),
            ("private suffix", self.PRIVATE, diagnostic + ": " + self.PRIVATE),
            ("leading space", self.PRIVATE, " " + diagnostic),
            ("trailing space", self.PRIVATE, diagnostic + " "),
            ("changed case", self.PRIVATE, diagnostic.upper()),
            ("quoted message", self.PRIVATE, '"' + diagnostic + '"'),
            ("private path", self.PRIVATE, "/" + self.PRIVATE + "/" + diagnostic),
            ("TCP ANSI text", self.PRIVATE, "\x1b[0;31m✗ preflight: ports already in use: 3000\x1b[0m"),
            ("resource text", self.PRIVATE, "⚠ <2 vCPU (1)"),
            ("logrotate path text", self.PRIVATE, "✗ logrotate not found at /" + self.PRIVATE),
            ("CRLF", self.PRIVATE, diagnostic + "\r\n"),
            ("embedded carriage return", self.PRIVATE, self.PRIVATE + "\r" + diagnostic),
            ("NUL before known line", self.PRIVATE, "\x00\n" + diagnostic),
            ("escape before known line", self.PRIVATE, "\x1b\n" + diagnostic),
            ("vertical-tab separator", self.PRIVATE, self.PRIVATE + "\v" + diagnostic),
            ("form-feed separator", self.PRIVATE, self.PRIVATE + "\f" + diagnostic),
            ("control in stdout", self.PRIVATE + "\x00", diagnostic),
            ("unsupported SGR in stdout", "\x1b[31m" + self.PRIVATE + "\x1b[0m", diagnostic),
            ("unsupported SGR in stderr", self.PRIVATE, "\x1b[31m" + self.PRIVATE + "\x1b[0m\n" + diagnostic),
            ("non-SGR escape", "\x1b[2J" + self.PRIVATE, diagnostic),
            ("surrogate in stdout", self.PRIVATE + "\ud800", diagnostic),
            ("surrogate in stderr", self.PRIVATE, diagnostic + "\n\ud800"),
        ]
        for label, stdout, stderr in cases:
            with self.subTest(scenario=label):
                self.assert_failed_installer(stdout, stderr, "port_posture")

    def test_conflicting_categories_retain_port_posture(self):
        for position, (first, _assertion) in enumerate(self.GUARDS):
            for second, _other in self.GUARDS[position + 1:]:
                with self.subTest(first=position, second=second):
                    self.assert_failed_installer(self.PRIVATE, first + "\n" + second,
                                                 "port_posture")

    def test_other_category_as_substring_does_not_conflict_with_one_exact_line(self):
        for position, (diagnostic, expected) in enumerate(self.GUARDS):
            decoy = self.GUARDS[(position + 1) % len(self.GUARDS)][0]
            stderr = diagnostic + "\n" + self.PRIVATE + ": " + decoy
            with self.subTest(assertion=expected):
                self.assert_failed_installer(self.PRIVATE, stderr, expected)

    def test_combined_utf8_byte_bound_is_inclusive_and_refuses_oversize_output(self):
        diagnostic, expected = self.GUARDS[0]
        for padding in ("X", "é"):
            for size in (8191, 8192, 8193):
                with self.subTest(padding=padding, bytes=size):
                    prefix = self.PRIVATE + "\n"
                    remaining = size - len((prefix + diagnostic).encode("utf-8"))
                    width = len(padding.encode("utf-8"))
                    stdout = prefix + padding * (remaining // width) + "X" * (remaining % width)
                    self.assertEqual(len((stdout + diagnostic).encode("utf-8")), size)
                    self.assert_failed_installer(stdout, diagnostic,
                                                 expected if size <= 8192 else "port_posture")



class MediaLaunchFailureEvidenceTests(unittest.TestCase):
    """Owned daemon fixtures exercise failed up and its serialized evidence.

    These observations distinguish finite launch boundaries; they do not establish
    the cause of a real CI failure or execute Docker/container operations.
    """

    HOSTED = AcceptanceRunnerTests.HOSTED
    setUp = AcceptanceRunnerTests.setUp
    runner = AcceptanceRunnerTests.runner
    run_admitted = AcceptanceRunnerTests.run_admitted
    reset_install_state = AcceptanceRunnerTests.reset_install_state
    assert_failure_evidence = AcceptanceRunnerTests.assert_failure_evidence

    PRIVATE = "PRIVATE_DAEMON_START_CANARY"
    TASK_PREFIX = ("failed to create task for container: failed to create shim task: "
                   "OCI runtime create failed: runc create failed: unable to start "
                   "container process: error during container init: ")
    BIND_ERROR = ("failed to set up container networking: driver failed programming "
                  "external connectivity on endpoint concordvoice-media-plane (" + "a" * 64 +
                  "): Bind for 127.0.0.1:3000 failed: port is already allocated")

    @staticmethod
    def projection(**changes):
        state = {"Status": "created", "Running": False, "Restarting": False,
                 "OOMKilled": False, "ExitCode": 0, "Health": None, "Error": ""}
        state.update(changes)
        return {"project": PROJECT, "service": "media-plane", "state": state}

    def assert_failed_up(self, projection, expected, *, logs="", expect_logs=True,
                         returncode=37):
        self.executor = _OwnedExecutor(self.workspace)
        self.executor.fail_startup = True
        self.executor.startup_returncode = returncode
        self.executor.startup_stdout = "voice-enforcement-rollout: waiting for media-plane liveness"
        self.executor.startup_inspect_result = acceptance.CommandResult(
            0, json.dumps(projection), "")
        self.executor.startup_logs_result = acceptance.CommandResult(0, logs, "")
        self.reset_install_state()
        output, error = io.StringIO(), io.StringIO()
        with mock.patch("sys.stdout", output), mock.patch("sys.stderr", error):
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                self.run_admitted()
        self.assertEqual(caught.exception.code, "COMMAND_FAILED")
        self.assertEqual(caught.exception.returncode, returncode)
        record = self.assert_failure_evidence("COMMAND_FAILED")
        self.assertEqual([check for check in record["checks"] if check["status"] == "FAIL"],
                         [{"stage": "observe", "assertion": expected,
                           "status": "FAIL", "code": "COMMAND_FAILED"}])
        serialized = json.dumps(record)
        public_output = output.getvalue() + error.getvalue()
        for private in (self.PRIVATE, self.executor.failure_sentinel, "foreign-owned-project",
                        "foreign-owned-service"):
            self.assertNotIn(private, serialized)
            self.assertNotIn(private, public_output)
        daemon_error = projection.get("state", {}).get("Error")
        if isinstance(daemon_error, str) and daemon_error.strip():
            self.assertFalse(daemon_error in serialized, "raw daemon error entered evidence")
            self.assertFalse(daemon_error in public_output, "raw daemon error entered caller output")
        self.assertFalse(any(check["status"] == "PASS" and check["stage"] == "observe"
                             for check in record["checks"]))
        self.assertFalse(any(event[0] == "route" for event in self.executor.events))
        self.assertEqual(self.executor.guard_count, {"log": 1, "tls": 1})
        self.assertEqual(self.executor.events.count(("startup-diagnostic", "inspect")), 1)
        self.assertEqual(self.executor.events.count(("startup-diagnostic", "logs")),
                         int(expect_logs))
        self.assertEqual(self.executor.events.count(("startup-failure", str(returncode))), 1)
        self.assertFalse(any(event[0] == "wrapper" and event[1] in {"stop", "down"}
                             for event in self.executor.events))

    def test_created_launch_errors_have_finite_daemon_categories(self):
        cases = [
            ("published port", self.BIND_ERROR, "startup_mp_daemon_bind"),
            ("permission", self.TASK_PREFIX + self.PRIVATE + ": permission denied: unknown",
             "startup_mp_daemon_permission"),
            ("missing executable", self.TASK_PREFIX +
             'exec: "node": executable file not found in $PATH: unknown', "startup_mp_daemon_oci"),
        ]
        cases += [(reason, self.TASK_PREFIX + self.PRIVATE + ": " + reason + ": unknown",
                   "startup_mp_daemon_resource")
                  for reason in ("resource temporarily unavailable", "cannot allocate memory",
                                 "no space left on device", "too many open files")]
        for label, daemon_error, expected in cases:
            with self.subTest(scenario=label):
                self.assert_failed_up(self.projection(Error=daemon_error), expected)

    def test_modern_daemon_bind_forms_validate_owned_port_tuples(self):
        # These complete owned fixtures use Moby 28.3.3's cfgPortBinding.String
        # spelling at the failed-up boundary, not a replay of a real CI error.
        prefix = ("failed to set up container networking: driver failed programming "
                  "external connectivity on endpoint concordvoice-media-plane (" + "b" * 64 + "): ")
        positives = {
            "IPv4 udp": "failed to bind host port for 0.0.0.0:40001:172.18.0.5:40001/udp",
            "chosen port in range": "failed to bind host port 40003 for 0.0.0.0:40001-40005:172.18.0.5:40001/udp",
            "IPv6 tcp": "failed to bind host port for [::]:40001:[2001:db8::5]:40001/tcp",
            "chosen IPv6 port": "failed to bind host port 40001 for [::1]:40001:[fd00::5]:40001/tcp",
        }
        for label, binding in positives.items():
            with self.subTest(valid_binding=label):
                self.assert_failed_up(self.projection(Error=prefix + binding + ": address already in use"),
                                      "startup_mp_daemon_bind")

        invalid = {
            "nonnumeric host": "failed to bind host port for example.invalid:40001:172.18.0.5:40001/udp",
            "invalid host IPv4": "failed to bind host port for 999.0.0.1:40001:172.18.0.5:40001/udp",
            "invalid container IPv4": "failed to bind host port for 0.0.0.0:40001:172.18.0.999:40001/udp",
            "invalid host IPv6": "failed to bind host port for [gg::1]:40001:[fd00::5]:40001/tcp",
            "invalid container IPv6": "failed to bind host port for [::]:40001:[2001:db8::gg]:40001/tcp",
            "unbracketed IPv6": "failed to bind host port for :::40001:fd00::5:40001/tcp",
            "nonnumeric host port": "failed to bind host port for 0.0.0.0:not-a-port:172.18.0.5:40001/udp",
            "zero host port": "failed to bind host port for 0.0.0.0:0:172.18.0.5:40001/udp",
            "overflow host port": "failed to bind host port for 0.0.0.0:65536:172.18.0.5:40001/udp",
            "zero container port": "failed to bind host port for 0.0.0.0:40001:172.18.0.5:0/udp",
            "overflow container port": "failed to bind host port for 0.0.0.0:40001:172.18.0.5:65536/udp",
            "nonnumeric chosen port": "failed to bind host port false for 0.0.0.0:40001:172.18.0.5:40001/udp",
            "chosen port outside range": "failed to bind host port 40006 for 0.0.0.0:40001-40005:172.18.0.5:40001/udp",
            "chosen port differs from singleton": "failed to bind host port 40002 for 0.0.0.0:40001:172.18.0.5:40001/udp",
            "reversed host range": "failed to bind host port 40003 for 0.0.0.0:40005-40001:172.18.0.5:40001/udp",
            "overflow range endpoint": "failed to bind host port 40003 for 0.0.0.0:40001-65536:172.18.0.5:40001/udp",
            "invalid protocol": "failed to bind host port for 0.0.0.0:40001:172.18.0.5:40001/icmp",
            "missing protocol": "failed to bind host port for 0.0.0.0:40001:172.18.0.5:40001",
            "missing container port": "failed to bind host port for 0.0.0.0:40001:172.18.0.5/udp",
        }
        for label, binding in invalid.items():
            with self.subTest(invalid_binding=label):
                self.assert_failed_up(self.projection(Error=prefix + binding + ": address already in use"),
                                      "startup_mp_created")
        known = prefix + positives["IPv4 udp"] + ": address already in use"
        for label, daemon_error in (("private prefix", self.PRIVATE + ": " + known),
                                    ("multiline", known + "\n" + self.PRIVATE),
                                    ("unrelated suffix", known + ": " + self.PRIVATE)):
            with self.subTest(decoy=label):
                self.assert_failed_up(self.projection(Error=daemon_error), "startup_mp_created")

    def test_empty_unknown_decoy_and_ambiguous_errors_preserve_created_state(self):
        missing = self.projection()
        del missing["state"]["Error"]
        cases = [("missing", missing), ("empty", self.projection()),
                 ("whitespace", self.projection(Error=" "))]
        decoys = {
            "unknown": self.PRIVATE + ": unknown launch failure",
            "keyword path": "/" + self.PRIVATE + "/permission denied/port is already allocated",
            "application record": json.dumps({"message": "Fatal error during startup",
                                                "error": {"code": "EACCES"}, "private": self.PRIVATE}),
            "prefixed daemon": self.PRIVATE + ": " + self.BIND_ERROR,
            "suffixed daemon": self.BIND_ERROR + ": " + self.PRIVATE,
            "multiline daemon": self.BIND_ERROR + "\n" + self.TASK_PREFIX +
                                self.PRIVATE + ": permission denied: unknown",
            "case changed": self.BIND_ERROR.upper(),
            "conflicting families": self.TASK_PREFIX + self.PRIVATE +
                                    ": permission denied: resource temporarily unavailable: unknown",
            "embedded unrelated failure": self.TASK_PREFIX + "/" + self.PRIVATE +
                                          "/permission denied: unrelated launch failure: unknown",
        }
        cases += [(label, self.projection(Error=daemon_error)) for label, daemon_error in decoys.items()]
        for label, projection in cases:
            with self.subTest(scenario=label):
                self.assert_failed_up(projection, "startup_mp_created")

    def test_daemon_error_types_and_utf8_bounds_fail_closed(self):
        for invalid in (None, True, 7, [self.PRIVATE], {"private": self.PRIVATE}, "\ud800"):
            with self.subTest(invalid_type=type(invalid).__name__, surrogate=isinstance(invalid, str)):
                self.assert_failed_up(self.projection(Error=invalid), "startup_mp_diag_invalid",
                                      expect_logs=False)
        for label, daemon_error, expected, expect_logs in (
                ("exact byte bound", "X" * 4096, "startup_mp_created", True),
                ("exceeds byte bound", "X" * 4097, "startup_mp_diag_limit", False),
                ("exact multibyte bound", "é" * 2048, "startup_mp_created", True),
                ("exceeds multibyte bound", "é" * 2049, "startup_mp_diag_limit", False)):
            with self.subTest(scenario=label):
                self.assert_failed_up(self.projection(Error=daemon_error), expected,
                                      expect_logs=expect_logs)

    def test_foreign_identity_prevents_daemon_error_interpretation(self):
        for key, value in (("project", "foreign-owned-project"),
                           ("service", "foreign-owned-service")):
            for daemon_error in (self.BIND_ERROR, {"private": self.PRIVATE}, "X" * 4097):
                with self.subTest(identity=key, error_type=type(daemon_error).__name__):
                    projection = self.projection(Error=daemon_error)
                    projection[key] = value
                    self.assert_failed_up(projection, "startup_mp_diag_identity", expect_logs=False)

    def test_daemon_error_does_not_reclassify_existing_state_or_oom(self):
        cases = [
            ("running", {"Status": "running", "Running": True}, "startup_mp_running", True),
            ("unhealthy", {"Status": "running", "Running": True,
                           "Health": {"Status": "unhealthy"}}, "startup_mp_unhealthy", True),
            ("restarting", {"Status": "restarting", "Running": True, "Restarting": True},
             "startup_mp_restarting", True),
            ("exited", {"Status": "exited", "ExitCode": 1}, "startup_mp_exited", True),
            ("oom", {"OOMKilled": True}, "startup_mp_oom", False),
            ("invalid created-running", {"Running": True}, "startup_mp_diag_invalid", False),
        ]
        for label, changes, expected, expect_logs in cases:
            for daemon_error in (self.BIND_ERROR, {"private": self.PRIVATE}, "X" * 4097):
                with self.subTest(scenario=label, error_type=type(daemon_error).__name__):
                    self.assert_failed_up(self.projection(Error=daemon_error, **changes), expected,
                                          expect_logs=expect_logs)

    def test_existing_fatal_log_precedence_and_original_failure_are_preserved(self):
        fatal = json.dumps({"message": "Fatal error during startup", "error": {"code": "ENOENT"},
                            "private": self.PRIVATE})
        self.assert_failed_up(self.projection(Error=self.BIND_ERROR), "startup_mp_log_missing_file",
                              logs=fatal, returncode=19)



class PublicBundleAcceptanceTests(unittest.TestCase):
    """Real owned archive/manifest bytes; host effects remain at the existing fake seam.

    Removing bundle admission, digest binding, or the shared lifecycle must break
    these tests. No fixture establishes staging provenance or public-host proof.
    """

    setUp = AcceptanceRunnerTests.setUp
    assert_no_mutations = AcceptanceRunnerTests.assert_no_mutations
    REQUIRED = Counter({
        ("host", "host_floors"): 2, ("tools", "tool_versions"): 1,
        ("render", "evidence_safety"): 1, ("render", "port_posture"): 1,
        ("render", "coturn_mount"): 2, ("render", "host_capacity"): 1,
        ("render", "local_only"): 2, ("observe", "selected_stack"): 1,
        ("observe", "port_posture"): 1, ("observe", "coturn_mount"): 1,
        ("observe", "disabled_agent"): 1, ("observe", "delivered_tls_metadata"): 1,
        ("observe", "served_tls_leaf"): 1, ("observe", "local_only"): 1,
        ("lifecycle", "selected_stack"): 2, ("lifecycle", "evidence_safety"): 3,
        ("lifecycle", "stopped_health"): 1, ("lifecycle", "data_survives_restart"): 2,
        ("lifecycle", "plain_down_preserves_volumes"): 1,
        ("privacy", "evidence_safety"): 2,
    })

    def make_bundle(self):
        destination = self.root / "concord-selfhost-1.2.3"
        self.workspace.rename(destination)
        self.workspace = destination
        (self.workspace / "docker-compose.production.yml").write_bytes(
            (ROOT / "scripts/public-mirror/public-overlay/docker-compose.production.yml").read_bytes())
        (self.workspace / "infrastructure/deploy/provision-production.sh").unlink()
        helper = "infrastructure/deploy/selfhost-security-event-logging.sh"
        (self.workspace / helper).write_bytes((ROOT / helper).read_bytes())
        files = {str(path.relative_to(self.workspace)): hashlib.sha256(path.read_bytes()).hexdigest()
                 for path in sorted(self.workspace.rglob("*")) if path.is_file()}
        manifest = {"schema": "selfhost-bundle/v1", "tag": "selfhost-v1.2.3",
                    "version": "1.2.3", "alpha_sha": "a" * 40, "public_sha": "b" * 40,
                    "root": self.workspace.name, "profile": "services", "files": files}
        inside = self.workspace / "selfhost-manifest.json"
        inside.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        # Model the public producer's canonical archive modes independently of
        # the host umask: source copies created by write_bytes are not installs.
        executable_paths = {
            "infrastructure/deploy/install-selfhost.sh",
            "infrastructure/deploy/concord-selfhost",
            "infrastructure/deploy/concord-ctl.sh",
            "infrastructure/deploy/provision-cert.sh",
            "infrastructure/deploy/selfhost-storage.sh",
            "infrastructure/deploy/selfhost-tls.sh",
            "infrastructure/deploy/selfhost-tls.py",
            "infrastructure/deploy/selfhost-security-event-logging.sh",
        }
        self.workspace.chmod(0o755)
        for path in self.workspace.rglob("*"):
            if path.is_dir():
                path.chmod(0o755)
            elif path.is_file():
                path.chmod(0o755 if str(path.relative_to(self.workspace)) in executable_paths else 0o644)
        self.manifest_path = self.root / "selfhost-manifest.json"
        self.manifest_path.write_bytes(inside.read_bytes())
        self.archive = self.root / "concord-selfhost-1.2.3.tar.gz"
        with tarfile.open(self.archive, "w:gz") as archive:
            archive.add(self.workspace, arcname=self.workspace.name)
        self.identity = {
            "schema": "selfhost-bundle-identity/v1", "alpha_sha": manifest["alpha_sha"],
            "public_sha": manifest["public_sha"],
            "bundle_sha256": hashlib.sha256(self.archive.read_bytes()).hexdigest(),
            "staging": {"run_id": 701, "parent_sha": "c" * 40, "artifact_sha256": "d" * 64},
            "run_id": 702, "archive_path": str(self.archive),
            "manifest_path": str(self.manifest_path),
        }
        self.identity_path = self.root / "bundle-identity.json"
        self.save_identity()
        self.executor = _OwnedExecutor(self.workspace)
        self.executor.public_bundle = True

    def save_identity(self):
        self.identity_path.write_text(json.dumps(self.identity), encoding="utf-8")

    def invoke_bundle(self, *, blocked=False, env_changes=None, include_identity=True):
        env = dict(AcceptanceRunnerTests.HOSTED, GITHUB_EVENT_NAME="workflow_dispatch",
                   GITHUB_REF="refs/heads/main", GITHUB_REF_NAME="main",
                   GITHUB_SHA="e" * 40, GITHUB_RUN_ID="702", RUNNER_TEMP=str(self.root))
        env.update(env_changes or {})
        argv = (["--blocked-prerequisite", "host-packages"] if blocked else ["--fresh-host"])
        argv += ["--evidence-dir", str(self.evidence)]
        if include_identity:
            argv += ["--bundle-identity", str(self.identity_path)]
        with mock.patch.dict(os.environ, env, clear=True), \
             mock.patch.object(Path, "cwd", return_value=self.workspace), \
             mock.patch.object(acceptance.os, "getuid", return_value=1000), \
             mock.patch.object(acceptance, "SubprocessExecutor", return_value=self.executor):
            return acceptance.main(argv)

    def record(self):
        return json.loads((self.evidence / "evidence.json").read_text(encoding="utf-8"))

    def assert_bundle_record(self, record):
        self.assertEqual(set(record), {"schema", "alpha_sha", "public_sha", "bundle_sha256",
                                      "staging", "run_id", "runner_os", "tool_versions",
                                      "local_only", "checks", "public_checks"})
        self.assertEqual(record["schema"], "selfhost-bundle-acceptance/v2")
        for key in ("alpha_sha", "public_sha", "bundle_sha256", "staging", "run_id"):
            self.assertEqual(record[key], self.identity[key], key)
        self.assertEqual(record["runner_os"], "ubuntu-24.04")
        self.assertIs(record["local_only"], True)
        self.assertEqual(record["public_checks"], "UNVERIFIED")
        self.assertEqual({entry.name for entry in self.evidence.iterdir()}, {"evidence.json"})
        self.assertFalse((self.evidence / "evidence.json").is_symlink())

    def assert_before_preparation(self):
        self.assert_no_mutations()
        self.assertFalse(any("selfhost-security-event-logging.sh" in " ".join(call)
                             for call in self.executor.calls))
        self.assertFalse((self.workspace / ".env").exists())

    def test_public_bundle_emits_exact_schema2_and_all_original_28_checks(self):
        self.make_bundle()
        self.assertEqual(self.invoke_bundle(), 0)
        record = self.record()
        self.assert_bundle_record(record)
        self.assertEqual(Counter((row["stage"], row["assertion"]) for row in record["checks"]),
                         self.REQUIRED)
        self.assertTrue(all(row["status"] == "PASS" and row["code"] == "OK"
                            for row in record["checks"]))
        self.assertFalse(any(call and call[0] == "git" for call in self.executor.calls),
                         "an unpacked archive has no Git HEAD identity")
        self.assertFalse((self.workspace / "infrastructure/deploy/provision-production.sh").exists())
        commands = [" ".join(call) for call in self.executor.calls]
        self.assertTrue(any("selfhost-security-event-logging.sh" in call for call in commands))
        self.assertFalse(any("provision-production.sh" in call for call in commands))
        self.assertIn(("provision-log", "security-event-logging"), self.executor.events)
        self.assertEqual([event[1] for event in self.executor.events if event[0] == "db-read"],
                         ["owned-row", "owned-row", "owned-row"])
        self.assertFalse(any("|| true" in command for command in commands))
        self.assertFalse(any("concordvoice-ops-agent" in call
                             for call in self.executor.calls if "inspect" in call))
        self.assertEqual((self.workspace / "docker-compose.production.yml").read_bytes(),
                         (ROOT / "scripts/public-mirror/public-overlay/docker-compose.production.yml").read_bytes())

    def test_public_bundle_preparation_reaches_real_logging_helper_argument_contract(self):
        self.make_bundle()
        helper = self.workspace / "infrastructure/deploy/selfhost-security-event-logging.sh"
        source = helper.read_text(encoding="utf-8")
        entry = re.search(r'(?ms)^if \[\[ "\$\{BASH_SOURCE\[0\]\}" == "\$0" \]\]; then\n.*\Z',
                          source)
        self.assertIsNotNone(entry, "the shipped logging helper direct-entry gate must be exercised")
        gate_source = entry.group()
        self.assertEqual(gate_source.count("[[ $EUID -ne 0 ]]"), 1,
                         "the owned gate must replace exactly one root identity check")
        gate_source = gate_source.replace("[[ $EUID -ne 0 ]]", '[[ "$OWNED_GATE_EUID" -ne 0 ]]')
        gate = self.root / "owned-logging-helper-entry.sh"
        receipt = self.root / "owned-logging-helper-install-receipt"
        # Keep the real verb, arity and root gate. Only its privileged install
        # effect is virtual: this harness cannot invoke host tools or paths.
        gate.write_text("install_security_event_logging() { "
                        "printf '%s\\n' prepared > \"$OWNED_GATE_RECEIPT\"; }\n" + gate_source,
                        encoding="utf-8")
        observed = []
        original_run = self.executor.run

        def execute_gate(arguments, uid="0"):
            return subprocess.run([TEST_BASH, str(gate), *arguments], cwd=self.root,
                                  env={"PATH": TEST_PATH, "LANG": "C", "LC_ALL": "C",
                                       "OWNED_GATE_EUID": uid, "OWNED_GATE_RECEIPT": str(receipt)},
                                  text=True, capture_output=True, timeout=5)

        def faithful_run(argv, timeout, input_text=None):
            actual = list(argv)
            if actual[:3] == ["sudo", "-n", "/usr/bin/python3"]:
                self.assertEqual(actual[3:5], ["-I", "-c"])
                self.assertEqual(actual[5], acceptance._PRIVILEGED_SUPERVISOR)
                actual = ["sudo", "-n", *actual[7:]]
            if str(helper) in actual:
                self.assertEqual(actual[:4], ["sudo", "-n", "bash", str(helper)])
                result = execute_gate(actual[4:])
                observed.append((actual, result))
                if result.returncode != 0:
                    return acceptance.CommandResult(result.returncode, result.stdout, result.stderr)
            return original_run(argv, timeout=timeout, input_text=input_text)

        with mock.patch.object(self.executor, "run", side_effect=faithful_run):
            result = self.invoke_bundle()
        self.assertEqual(len(observed), 1, "the admitted bundle caller must reach the direct helper once")
        actual, child = observed[0]
        self.assertEqual(child.returncode, 0, child.stdout + child.stderr)
        self.assertEqual(result, 0, "the helper contract must allow the full public acceptance lifecycle")
        self.assertEqual(actual[4:], ["security-event-logging"])
        self.assertEqual(receipt.read_text(), "prepared\n")
        self.assert_bundle_record(self.record())
        self.assertEqual(Counter((row["stage"], row["assertion"]) for row in self.record()["checks"]),
                         self.REQUIRED)
        # These controls spend the actual caller's suffix through the same gate;
        # no separately hand-authored valid invocation can hide a broken caller.
        for arguments, uid, diagnostic in ((actual[4:][1:], "0", "Usage:"),
                                            (actual[4:] + ["unexpected"], "0", "takes no arguments"),
                                            (actual[4:], "1000", "must be run as root")):
            with self.subTest(arguments=arguments, uid=uid):
                receipt.unlink(missing_ok=True)
                refused = execute_gate(arguments, uid)
                self.assertNotEqual(refused.returncode, 0)
                self.assertIn(diagnostic, refused.stdout + refused.stderr)
                self.assertFalse(receipt.exists(), "refused helper invocation must not reach installation")

    def test_public_bundle_rejects_extra_project_service_metadata(self):
        for service in ("ops-agent", "foreign-service"):
            with self.subTest(service=service):
                self.setUp()
                self.make_bundle()
                self.executor.public_extra_service = service
                self.assertNotEqual(self.invoke_bundle(), 0)
                self.assertTrue(any(row["status"] == "FAIL" for row in self.record()["checks"]))
                self.assertFalse(any(row["assertion"] == "disabled_agent" and row["status"] == "PASS"
                                     for row in self.record()["checks"]))

    def test_substituted_bundle_identity_and_payload_refuse_before_preparation(self):
        cases = ("archive", "alpha", "public", "payload", "standalone-manifest",
                 "extra-payload", "managed-provisioner", "missing-helper", "linked-payload")
        for case in cases:
            with self.subTest(case=case):
                self.setUp()
                self.make_bundle()
                if case == "archive":
                    self.archive.write_bytes(b"substituted archive")
                elif case in {"alpha", "public"}:
                    self.identity[case + "_sha"] = "f" * 40
                    self.save_identity()
                elif case == "payload":
                    (self.workspace / "services/media-plane/Dockerfile").write_text("substituted payload\n")
                elif case == "standalone-manifest":
                    self.manifest_path.write_bytes(self.manifest_path.read_bytes() + b" ")
                elif case == "extra-payload":
                    (self.workspace / "unexpected-input").write_text("unlisted\n")
                elif case == "managed-provisioner":
                    (self.workspace / "infrastructure/deploy/provision-production.sh").write_text("managed\n")
                elif case == "missing-helper":
                    (self.workspace / "infrastructure/deploy/selfhost-security-event-logging.sh").unlink()
                else:
                    target = self.workspace / "services/media-plane/Dockerfile"
                    target.unlink()
                    target.symlink_to(ROOT / "services/media-plane/Dockerfile")
                self.assertNotEqual(self.invoke_bundle(), 0)
                self.assert_before_preparation()
                record = self.record()
                self.assertEqual(record["public_checks"], "UNVERIFIED")
                self.assertTrue(any(row["status"] == "FAIL" and row["code"] == "BUNDLE_IDENTITY_INVALID"
                                    for row in record["checks"]))
                self.assertFalse(any(row["status"] == "PASS" for row in record["checks"]))

    def test_missing_identity_cannot_turn_dispatch_into_pr_admission(self):
        self.make_bundle()
        self.assertNotEqual(self.invoke_bundle(include_identity=False), 0)
        self.assert_before_preparation()
        self.identity_path.unlink()
        self.assertNotEqual(self.invoke_bundle(), 0)
        self.assert_before_preparation()

    def test_unpacked_permission_drift_refuses_before_preparation(self):
        # The valid control is test_public_bundle_emits_exact_schema2_and_all_original_28_checks.
        # Neither payload bytes nor the pinned archive changes here: admission
        # must bind the unpacked filesystem mode to its corresponding tar member.
        for relative, change in (
                ("infrastructure/deploy/selfhost-security-event-logging.sh", "execute"),
                ("infrastructure/deploy/selfhost-security-event-logging.sh", "writable"),
                ("services/media-plane/Dockerfile", "writable")):
            with self.subTest(relative=relative, change=change):
                self.setUp()
                self.make_bundle()
                payload = self.workspace / relative
                original_bytes = payload.read_bytes()
                archive_digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
                with tarfile.open(self.archive, "r:gz") as archive:
                    original_mode = archive.getmember(self.workspace.name + "/" + relative).mode & 0o7777
                self.assertEqual(payload.stat().st_mode & 0o7777, original_mode)
                changed_mode = original_mode ^ (0o100 if change == "execute" else 0o002)
                payload.chmod(changed_mode)
                self.assertEqual(payload.read_bytes(), original_bytes)
                self.assertEqual(hashlib.sha256(self.archive.read_bytes()).hexdigest(), archive_digest)
                self.assertNotEqual(payload.stat().st_mode & 0o7777, original_mode)
                self.assertNotEqual(self.invoke_bundle(), 0)
                self.assert_before_preparation()
                record = self.record()
                self.assertEqual(record["public_checks"], "UNVERIFIED")
                self.assertTrue(any(row["status"] == "FAIL" and row["code"] == "BUNDLE_IDENTITY_INVALID"
                                    for row in record["checks"]))
                self.assertFalse(any(row["status"] == "PASS" for row in record["checks"]))

    def test_matching_archive_digest_does_not_admit_unsafe_member_inventory(self):
        for kind in ("traversal", "absolute", "symlink", "hardlink", "duplicate"):
            with self.subTest(kind=kind):
                self.setUp()
                self.make_bundle()
                with tarfile.open(self.archive, "w:gz") as archive:
                    archive.add(self.workspace, arcname=self.workspace.name)
                    if kind == "duplicate":
                        archive.add(self.workspace / "selfhost-manifest.json",
                                    arcname=self.workspace.name + "/selfhost-manifest.json")
                    else:
                        name = {"traversal": "../escaped", "absolute": "/escaped"}.get(
                            kind, self.workspace.name + "/unsafe-link")
                        member = tarfile.TarInfo(name)
                        if kind in {"symlink", "hardlink"}:
                            member.type = tarfile.SYMTYPE if kind == "symlink" else tarfile.LNKTYPE
                            member.linkname = "../outside"
                            archive.addfile(member)
                        else:
                            member.size = 1
                            archive.addfile(member, io.BytesIO(b"x"))
                self.identity["bundle_sha256"] = hashlib.sha256(self.archive.read_bytes()).hexdigest()
                self.save_identity()
                self.assertNotEqual(self.invoke_bundle(), 0)
                self.assert_before_preparation()
                self.assertFalse((self.root / "escaped").exists())
                self.assertTrue(any(row["status"] == "FAIL" and row["code"] == "BUNDLE_IDENTITY_INVALID"
                                    for row in self.record()["checks"]))

    def test_identity_closed_shape_and_regular_paths_refuse_before_preparation(self):
        for case in ("schema1", "unknown", "missing-staging", "bool-run", "uppercase-sha",
                     "bad-digest", "duplicate-key", "linked-archive", "linked-manifest",
                     "linked-identity"):
            with self.subTest(case=case):
                self.setUp()
                self.make_bundle()
                if case == "schema1":
                    self.identity["schema"] = 1
                elif case == "unknown":
                    self.identity["verified"] = True
                elif case == "missing-staging":
                    del self.identity["staging"]
                elif case == "bool-run":
                    self.identity["staging"]["run_id"] = True
                elif case == "uppercase-sha":
                    self.identity["alpha_sha"] = "A" * 40
                elif case == "bad-digest":
                    self.identity["bundle_sha256"] = "a" * 63
                self.save_identity()
                if case == "duplicate-key":
                    text = self.identity_path.read_text()
                    self.identity_path.write_text(text[:-1] + ', "run_id": 702}')
                elif case.startswith("linked-"):
                    path = {"linked-archive": self.archive, "linked-manifest": self.manifest_path,
                            "linked-identity": self.identity_path}[case]
                    destination = self.root / (path.name + ".real")
                    path.rename(destination)
                    path.symlink_to(destination)
                self.assertNotEqual(self.invoke_bundle(), 0)
                self.assert_before_preparation()

    def test_bundle_hosted_claims_cannot_replace_explicit_release_mode(self):
        for changes in ({"GITHUB_EVENT_NAME": "pull_request"},
                        {"GITHUB_REF": "refs/heads/feature/unreviewed", "GITHUB_REF_NAME": "feature/unreviewed"},
                        {"GITHUB_RUN_ID": "703"}, {"RUNNER_ENVIRONMENT": "self-hosted"}):
            with self.subTest(changes=changes):
                self.setUp()
                self.make_bundle()
                self.assertNotEqual(self.invoke_bundle(env_changes=changes), 0)
                self.assert_before_preparation()

    def test_bundle_lifecycle_failure_keeps_schema2_without_public_pass(self):
        self.make_bundle()
        self.executor.lost_readback = "down"
        self.assertNotEqual(self.invoke_bundle(), 0)
        record = self.record()
        self.assert_bundle_record(record)
        self.assertTrue(any(row["code"] == "DATA_SURVIVAL_INVALID" and row["status"] == "FAIL"
                            for row in record["checks"]))
        self.assertFalse(any(row["assertion"] == "plain_down_preserves_volumes" and row["status"] == "PASS"
                             for row in record["checks"]))

    def test_blacksmith_jit_profile_runs_exact_bundle_schema2_checks(self):
        self.make_bundle()
        self.assertEqual(self.invoke_bundle(env_changes=AcceptanceRunnerTests.BLACKSMITH_PROFILE), 0)
        record = self.record()
        self.assert_bundle_record(record)
        self.assertEqual(Counter((row["stage"], row["assertion"]) for row in record["checks"]), self.REQUIRED)
        self.assertTrue(all(row["status"] == "PASS" and row["code"] == "OK" for row in record["checks"]))
        self.assertEqual([event[1] for event in self.executor.events if event[0] == "db-read"],
                         ["owned-row", "owned-row", "owned-row"])

    def test_blacksmith_bundle_fresh_and_blocked_modes_refuse_invalid_profile_pairs(self):
        for blocked in (False, True):
            for change in (None, {"SELFHOST_ACCEPTANCE_RUNNER_PROFILE": ""},
                           {"SELFHOST_ACCEPTANCE_RUNNER_PROFILE": "unknown"},
                           {"GITHUB_REPOSITORY": "untrusted/fixture"},
                           {"RUNNER_ENVIRONMENT": "github-hosted"}, {"RUNNER_ENVIRONMENT": "unknown"},
                           {"RUNNER_ENVIRONMENT": "github-hosted", "SELFHOST_ACCEPTANCE_RUNNER_PROFILE": ""}):
                with self.subTest(blocked=blocked, change=change):
                    self.setUp()
                    self.make_bundle()
                    env = dict(AcceptanceRunnerTests.BLACKSMITH_PROFILE, **(change or {}))
                    if change is None:
                        env.pop("SELFHOST_ACCEPTANCE_RUNNER_PROFILE")
                    self.assertEqual(self.invoke_bundle(blocked=blocked, env_changes=env), 1)
                    self.assertEqual(self.executor.calls, [])
                    self.assert_before_preparation()

    def test_blacksmith_bundle_blocked_recorder_preserves_schema2_identities(self):
        self.make_bundle()
        self.assertEqual(self.invoke_bundle(blocked=True, env_changes=AcceptanceRunnerTests.BLACKSMITH_PROFILE), 0)
        record = self.record()
        self.assert_bundle_record(record)
        self.assertEqual(record["checks"], [{"stage": "host", "assertion": "host_floors",
                                            "status": "BLOCKED", "code": "HOST_PREREQUISITES_FAILED"}])
        self.assertEqual(self.executor.calls, [])
        self.assert_before_preparation()

    def test_blocked_bundle_host_packages_emit_schema2_without_running_checks(self):
        self.make_bundle()
        self.assertEqual(self.invoke_bundle(blocked=True), 0)
        record = self.record()
        self.assert_bundle_record(record)
        self.assertEqual(record["checks"], [{"stage": "host", "assertion": "host_floors",
                                            "status": "BLOCKED", "code": "HOST_PREREQUISITES_FAILED"}])
        self.assert_before_preparation()
        self.assertFalse(any(call and call[0] == "git" for call in self.executor.calls))



if __name__ == "__main__":
    unittest.main(verbosity=2)
